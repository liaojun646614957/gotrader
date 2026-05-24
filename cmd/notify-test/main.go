// notify-test：读 config.yaml 的 notify 段，发一封测试邮件。
//
// 用法：
//
//	go run ./cmd/notify-test -config config.yaml
//
// 同步发送，SMTP 错误直接打印——上线前先用这个工具确认通道通畅。
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kraus/gotrader/internal/config"
	"github.com/kraus/gotrader/internal/notify"
)

func main() {
	configPath := flag.String("config", "config.yaml", "config 文件路径")
	flag.Parse()

	cfg, err := config.LoadForBacktest(*configPath)
	if err != nil {
		// 用 LoadForBacktest：不强制 OKX 凭证，只要能解析 yaml 就行
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	if !cfg.Notify.Enabled() {
		fmt.Fprintln(os.Stderr, "notify 配置未启用 / 不完整，检查 config.yaml 的 notify 段")
		os.Exit(1)
	}

	fmt.Printf("发送测试邮件 host=%s port=%d from=%s to=%v use_tls=%v\n",
		cfg.Notify.Host, cfg.Notify.Port, cfg.Notify.From, cfg.Notify.To, cfg.Notify.UseTLS)

	subject := "[gotrader] SMTP 通道测试"
	body := "这是一封测试邮件。\n\n" +
		"如果你收到这封邮件，说明 gotrader 邮件通知通道已经打通，\n" +
		"策略产生信号、下单成功或被风控拦截时，你会收到类似格式的邮件。\n\n" +
		"发送时间: " + time.Now().Format("2006-01-02 15:04:05 -0700") + "\n"

	start := time.Now()
	if err := notify.SendSync(cfg.Notify, subject, body); err != nil {
		fmt.Fprintf(os.Stderr, "发送失败 (%.2fs): %v\n", time.Since(start).Seconds(), err)
		os.Exit(1)
	}
	fmt.Printf("发送成功，用时 %.2fs。请到收件箱（含垃圾邮件夹）检查。\n", time.Since(start).Seconds())
}
