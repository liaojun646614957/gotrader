// Package notify 信号 / 订单事件的旁路通知。
//
// 设计原则：
//   - 旁路、异步、绝不阻塞主交易循环。Notify() 内部自己起 goroutine。
//   - 失败只 log warn，不传播 error。通知挂掉不应该影响下单。
//   - 没配 SMTP 走 Noop 实现，调用点不需要 if nil 判断（消除特殊情况）。
//
// 当前只实现 SMTP（标准库 net/smtp，零三方依赖）。
// 未来加 Webhook / Telegram 只是新增一个 Notifier 实现，不改调用方。
package notify

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Notifier 通知器。所有实现必须保证 Notify 不阻塞调用者。
type Notifier interface {
	// Notify 发送一条通知。subject 短，body 可多行。
	// 实现内部异步处理；调用方不需要起 goroutine。
	Notify(subject, body string)
}

// SMTPConfig SMTP 邮件通知配置。
//
// 示例（推荐用 QQ/163/Gmail 作发件，把通知送到 iCloud 收件箱）：
//
//	host: smtp.qq.com
//	port: 587
//	username: your_qq@qq.com
//	password: <授权码，不是登录密码>
//	from:     your_qq@qq.com
//	to:       [liaojun646614957@icloud.com]
//
// 注意：iCloud Mail 作发件需要 App-Specific Password（不是 iCloud 登录密码）。
type SMTPConfig struct {
	Host     string   `yaml:"host"`
	Port     int      `yaml:"port"`     // 587 (STARTTLS) 或 465 (SSL)
	Username string   `yaml:"username"`
	Password string   `yaml:"password"` // 邮箱授权码 / App password
	From     string   `yaml:"from"`     // 发件人地址（一般等于 username）
	To       []string `yaml:"to"`       // 收件人列表
	UseTLS   bool     `yaml:"use_tls"`  // true=SSL(465), false=STARTTLS(587)
}

// Enabled 配置是否完整可用。任何关键字段缺失视为禁用。
func (c SMTPConfig) Enabled() bool {
	return c.Host != "" && c.Port > 0 && c.Username != "" && c.Password != "" &&
		c.From != "" && len(c.To) > 0
}

// New 根据配置返回 Notifier。配置未启用时返回 Noop（永远不返回 nil）。
func New(cfg SMTPConfig) Notifier {
	if !cfg.Enabled() {
		slog.Info("通知未启用：SMTP 配置缺失（信号将仅记录日志）")
		return Noop{}
	}
	slog.Info("通知已启用", "host", cfg.Host, "from", cfg.From, "to", cfg.To)
	return &smtpNotifier{cfg: cfg}
}

// Noop 空实现：不做任何事。
type Noop struct{}

func (Noop) Notify(subject, body string) {}

// smtpNotifier SMTP 邮件实现。每次 Notify 起一个 goroutine 异步发送。
//
// 没做队列 / 限流 / 重试——量化策略信号频率本就低（TSMOM 5 天一次）。
// 真要海量信号那是策略有问题，不是通知系统的锅。
type smtpNotifier struct {
	cfg SMTPConfig
}

func (s *smtpNotifier) Notify(subject, body string) {
	go func() {
		if err := s.send(subject, body); err != nil {
			slog.Warn("邮件通知发送失败", "subject", subject, "err", err)
		}
	}()
}

// SendSync 同步发送一封邮件并返回错误。
//
// **仅供 CLI 工具 / 单元测试使用**——主交易循环必须走 Notifier.Notify 异步路径，
// 否则 SMTP 网络抖动会直接卡死信号处理。
func SendSync(cfg SMTPConfig, subject, body string) error {
	if !cfg.Enabled() {
		return fmt.Errorf("SMTP 配置不完整（host/port/username/password/from/to 都需填写）")
	}
	s := &smtpNotifier{cfg: cfg}
	return s.send(subject, body)
}

// send 同步发送。15 秒整体超时——超过就放弃，避免 goroutine 泄漏。
func (s *smtpNotifier) send(subject, body string) error {
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))
	msg := buildMessage(s.cfg.From, s.cfg.To, subject, body)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	if s.cfg.UseTLS {
		// 465: 隐式 SSL
		tlsConn := tls.Client(conn, &tls.Config{ServerName: s.cfg.Host})
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("tls handshake: %w", err)
		}
		return s.deliver(tlsConn, msg)
	}
	// 587: 明文连接，连接后用 STARTTLS 升级
	return s.deliver(conn, msg)
}

func (s *smtpNotifier) deliver(conn net.Conn, msg []byte) error {
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp new client: %w", err)
	}
	defer c.Close()

	if !s.cfg.UseTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host}); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		}
	}

	auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	if err := c.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := c.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}
	for _, to := range s.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}
	return c.Quit()
}

// buildMessage 构造符合 RFC 5322 的最小邮件。中文 subject 用 MIME B-encoding。
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	b.WriteString("From: ")
	b.WriteString(from)
	b.WriteString("\r\n")
	b.WriteString("To: ")
	b.WriteString(strings.Join(to, ", "))
	b.WriteString("\r\n")
	b.WriteString("Subject: ")
	b.WriteString(encodeSubject(subject))
	b.WriteString("\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("Date: ")
	b.WriteString(time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("\r\n\r\n")
	b.WriteString(body)
	b.WriteString("\r\n")
	return []byte(b.String())
}

// encodeSubject 含非 ASCII 字符时走 RFC 2047 B-encoding，否则原样返回。
func encodeSubject(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}
