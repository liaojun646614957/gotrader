package okx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kraus/gotrader/internal/types"
)

// WSChannel 表示一个订阅频道。
type WSChannel struct {
	Channel string `json:"channel"`         // e.g. "tickers" / "candle1m" / "orders"
	InstID  string `json:"instId,omitempty"`
	InstType string `json:"instType,omitempty"`
}

// WSEvent WS 推送事件。Type 决定具体载荷字段，调用方按需取。
type WSEvent struct {
	Channel string
	Kline   *types.Kline // candleXX 频道
	Tick    *types.Tick  // trades 频道
	Order   *types.Order // orders 频道（私有）
	Raw     json.RawMessage // 兜底，调用方自己解
}

// WSClient 单一连接的 WS 客户端。一个进程通常需要 3 个：public/private/business。
//
// 设计取舍：
//   - 不做"订阅热重载"。要新加订阅就重建连接。复杂度收益不成正比。
//   - 重连用指数退避，最大 30s。订阅在重连后自动恢复。
//   - 心跳：OKX 30s 内无消息会断，我们 25s 主动发 "ping"。
type WSClient struct {
	url        string
	private    bool
	apiKey     string
	secretKey  string
	passphrase string

	mu          sync.Mutex
	conn        *websocket.Conn
	channels    []WSChannel // 当前订阅，重连时回放
	events      chan WSEvent
	stopCh      chan struct{}
	stoppedCh   chan struct{}
	connected   bool
}

// NewWSClient 构造一个 WS 客户端。private=true 会在连接后自动登录。
// apiKey/secretKey/passphrase 仅 private=true 时使用。
func NewWSClient(url string, private bool, apiKey, secretKey, passphrase string) *WSClient {
	return &WSClient{
		url:        url,
		private:    private,
		apiKey:     apiKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		events:     make(chan WSEvent, 1024),
		stopCh:     make(chan struct{}),
		stoppedCh:  make(chan struct{}),
	}
}

// Events 返回事件 channel。调用方负责消费，慢消费会丢消息（buffered=1024）。
func (w *WSClient) Events() <-chan WSEvent { return w.events }

// Subscribe 添加订阅。可在 Run 之前或之后调用。
func (w *WSClient) Subscribe(chs ...WSChannel) {
	w.mu.Lock()
	w.channels = append(w.channels, chs...)
	conn := w.conn
	connected := w.connected
	w.mu.Unlock()

	if connected && conn != nil {
		_ = w.sendSubscribe(conn, chs)
	}
}

// Run 阻塞运行。返回时 stopCh 已关闭或 ctx done。调用方一般 go w.Run(ctx)。
func (w *WSClient) Run(ctx context.Context) {
	defer close(w.stoppedCh)

	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stopCh:
			return
		default:
		}

		if err := w.runOnce(ctx); err != nil {
			// 主动关闭（Stop/ctx done）和真实断网走的是同一条 error 路径。
			// 这里先甄别一下：是我们自己关的就安静退出，别误报。
			select {
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			default:
			}
			slog.Warn("WS 已断开，重连中", "url", w.url, "err", err, "backoff", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			}
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}
		backoff = time.Second
	}
}

// Stop 触发优雅关闭。Run 退出后 stoppedCh 关闭。
func (w *WSClient) Stop() {
	close(w.stopCh)
	w.mu.Lock()
	conn := w.conn
	w.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
	<-w.stoppedCh
}

// runOnce 跑一次连接生命周期，连接断了就 return error。
func (w *WSClient) runOnce(ctx context.Context) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, w.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	w.mu.Lock()
	w.conn = conn
	w.connected = true
	channels := append([]WSChannel(nil), w.channels...)
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.connected = false
		w.conn = nil
		w.mu.Unlock()
		_ = conn.Close()
	}()

	if w.private {
		if err := w.login(conn); err != nil {
			return fmt.Errorf("login: %w", err)
		}
	}
	if len(channels) > 0 {
		if err := w.sendSubscribe(conn, channels); err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}
	}

	// 心跳
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go w.pingLoop(pingCtx, conn)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.stopCh:
			return nil
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(40 * time.Second))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		if string(msg) == "pong" {
			continue
		}
		w.dispatch(msg)
	}
}

func (w *WSClient) pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(25 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = conn.WriteMessage(websocket.TextMessage, []byte("ping"))
		}
	}
}

// login 私有频道登录。详见 OKX 文档 "WebSocket 登录"。
// 签名 path 固定为 "/users/self/verify"，body 空。timestamp 用秒（带小数）。
func (w *WSClient) login(conn *websocket.Conn) error {
	tsSec := strconv.FormatInt(time.Now().Unix(), 10)
	signature := sign(w.secretKey, tsSec, "GET", "/users/self/verify", "")
	req := map[string]interface{}{
		"op": "login",
		"args": []map[string]string{
			{
				"apiKey":     w.apiKey,
				"passphrase": w.passphrase,
				"timestamp":  tsSec,
				"sign":       signature,
			},
		},
	}
	if err := conn.WriteJSON(req); err != nil {
		return err
	}
	// 等待登录响应
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	var resp struct {
		Event string `json:"event"`
		Code  string `json:"code"`
		Msg   string `json:"msg"`
	}
	if err := json.Unmarshal(msg, &resp); err != nil {
		return fmt.Errorf("decode login: %w (raw=%s)", err, string(msg))
	}
	if resp.Code != "0" {
		return fmt.Errorf("login rejected: code=%s msg=%s", resp.Code, resp.Msg)
	}
	return nil
}

func (w *WSClient) sendSubscribe(conn *websocket.Conn, chs []WSChannel) error {
	req := map[string]interface{}{
		"op":   "subscribe",
		"args": chs,
	}
	return conn.WriteJSON(req)
}

// rawPush OKX 推送统一外层。
type rawPush struct {
	Arg  WSChannel       `json:"arg"`
	Data json.RawMessage `json:"data"`
	Event string         `json:"event"` // 订阅响应 / 错误
	Code  string         `json:"code"`
	Msg   string         `json:"msg"`
}

func (w *WSClient) dispatch(msg []byte) {
	var p rawPush
	if err := json.Unmarshal(msg, &p); err != nil {
		slog.Warn("WS 解码失败", "err", err, "raw", string(msg))
		return
	}
	if p.Event != "" {
		// subscribe 响应或错误
		if p.Code != "" && p.Code != "0" {
			slog.Warn("WS 事件错误", "code", p.Code, "msg", p.Msg)
		}
		return
	}

	ev := WSEvent{Channel: p.Arg.Channel, Raw: p.Data}

	switch {
	case p.Arg.Channel == "trades":
		w.parseTrades(p, &ev)
	case len(p.Arg.Channel) >= 6 && p.Arg.Channel[:6] == "candle":
		w.parseKline(p, &ev)
	case p.Arg.Channel == "orders":
		w.parseOrder(p, &ev)
	}

	select {
	case w.events <- ev:
	default:
		slog.Warn("WS 事件 channel 已满，丢弃", "channel", p.Arg.Channel)
	}
}

func (w *WSClient) parseTrades(p rawPush, ev *WSEvent) {
	var arr []struct {
		InstID string `json:"instId"`
		TS     string `json:"ts"`
		Px     string `json:"px"`
		Sz     string `json:"sz"`
		Side   string `json:"side"`
	}
	if err := json.Unmarshal(p.Data, &arr); err != nil || len(arr) == 0 {
		return
	}
	t := arr[len(arr)-1]
	ts, _ := strconv.ParseInt(t.TS, 10, 64)
	px, _ := strconv.ParseFloat(t.Px, 64)
	sz, _ := strconv.ParseFloat(t.Sz, 64)
	ev.Tick = &types.Tick{
		InstID:    t.InstID,
		Timestamp: ts,
		Price:     px,
		Size:      sz,
		Side:      types.Side(t.Side),
	}
}

func (w *WSClient) parseKline(p rawPush, ev *WSEvent) {
	var arr [][]string
	if err := json.Unmarshal(p.Data, &arr); err != nil || len(arr) == 0 {
		return
	}
	r := arr[0]
	if len(r) < 6 {
		return
	}
	// OKX candle 频道格式: [ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm]
	// confirm: "0"=K线未完成（盘中实时更新，可能每秒几次），"1"=K线已完成（最终值）
	// 策略只应接收"已完成"的 K 线，否则同一根 K 线会被算成 N 根，
	// 计数器（如 TSMOM 的 barsSinceEval）会被打乱。
	if len(r) >= 9 && r[8] != "1" {
		return // 未完成 K 线，跳过（ev.Kline 留空，dispatch 不会推送给策略）
	}
	ts, _ := strconv.ParseInt(r[0], 10, 64)
	o, _ := strconv.ParseFloat(r[1], 64)
	h, _ := strconv.ParseFloat(r[2], 64)
	l, _ := strconv.ParseFloat(r[3], 64)
	cl, _ := strconv.ParseFloat(r[4], 64)
	v, _ := strconv.ParseFloat(r[5], 64)
	ev.Kline = &types.Kline{
		InstID:    p.Arg.InstID,
		Timestamp: ts,
		Open:      o,
		High:      h,
		Low:       l,
		Close:     cl,
		Volume:    v,
	}
}

func (w *WSClient) parseOrder(p rawPush, ev *WSEvent) {
	var arr []rawOrder
	if err := json.Unmarshal(p.Data, &arr); err != nil || len(arr) == 0 {
		return
	}
	ev.Order = arr[0].toOrder()
}
