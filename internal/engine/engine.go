// Package engine 事件驱动主循环。
//
// 数据流（这是整个系统的核心，看明白这个图就懂了一半）：
//
//	    OKX WS ─┐
//	            ├──► events ch ──► dispatch ──► strategy.OnXxx ──► signals ch
//	    OKX WS ─┘                                                       │
//	                                                                    ▼
//	                                                   risk.Check ─► OKX REST place
//
// 单 goroutine 跑 dispatch + place（避免锁）；WS reader 各自一个 goroutine。
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kraus/gotrader/internal/exchange/okx"
	"github.com/kraus/gotrader/internal/notify"
	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

// Engine 主循环。
type Engine struct {
	rest         *okx.Client
	wsPub        *okx.WSClient // 行情（trades）
	wsBiz        *okx.WSClient // K 线（candle 频道在 business 端）
	wsPriv       *okx.WSClient // 订单 / 持仓
	risk         *RiskGuard
	strat        strategy.Strategy
	stratCfg     strategy.Config
	notifier     notify.Notifier // 信号 / 下单事件的邮件通知。无配置时为 Noop。
	positionMode string          // "net"（单向，OKX 默认）/ "long_short"（双向）。决定下单是否带 posSide。

	// 市场状态缓存：最新成交价、持仓金额
	mu             sync.RWMutex
	lastPrice      map[string]float64 // instID -> price
	positionValue  map[string]float64 // instID -> 持仓 USDT 估值
}

// New 构造 Engine。notifier 传 nil 等价于 notify.Noop{}（永远不会 panic）。
// positionMode 留空 → 默认 "net"（单向持仓）。
func New(rest *okx.Client, wsPub, wsBiz, wsPriv *okx.WSClient,
	risk *RiskGuard, strat strategy.Strategy, stratCfg strategy.Config,
	notifier notify.Notifier, positionMode string) *Engine {
	if notifier == nil {
		notifier = notify.Noop{}
	}
	if positionMode == "" {
		positionMode = "net"
	}
	return &Engine{
		rest:          rest,
		wsPub:         wsPub,
		wsBiz:         wsBiz,
		wsPriv:        wsPriv,
		risk:          risk,
		strat:         strat,
		stratCfg:      stratCfg,
		notifier:      notifier,
		positionMode:  positionMode,
		lastPrice:     make(map[string]float64),
		positionValue: make(map[string]float64),
	}
}

// Run 阻塞运行直到 ctx done。
func (e *Engine) Run(ctx context.Context) error {
	if err := e.bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	signals := make(chan types.Signal, 64)
	if err := e.strat.Init(e.stratCfg, signals); err != nil {
		return fmt.Errorf("strategy init: %w", err)
	}

	// 预热：必须在 Init 之后（策略参数已就绪）、WS 启动之前（避免实时事件穿插）
	e.warmup(signals)

	// 启动三个 WS
	go e.wsPub.Run(ctx)
	go e.wsBiz.Run(ctx)
	go e.wsPriv.Run(ctx)

	e.subscribe()

	// 主循环：单 goroutine 处理所有事件，无锁分发
	for {
		select {
		case <-ctx.Done():
			slog.Info("引擎停止中")
			e.wsPub.Stop()
			e.wsBiz.Stop()
			e.wsPriv.Stop()
			return nil

		case ev := <-e.wsPub.Events():
			e.handleEvent(ev)
		case ev := <-e.wsBiz.Events():
			e.handleEvent(ev)
		case ev := <-e.wsPriv.Events():
			e.handleEvent(ev)

		case sig := <-signals:
			e.handleSignal(sig)
		}
	}
}

// bootstrap 启动前同步一次账户/持仓状态。失败直接退出，因为没有这些状态风控算不准。
func (e *Engine) bootstrap(ctx context.Context) error {
	_ = ctx
	positions, err := e.rest.GetPositions("")
	if err != nil {
		return fmt.Errorf("get positions: %w", err)
	}
	for _, p := range positions {
		// 用开仓均价估算持仓 USDT 金额（张数 → 名义 USDT，必须乘 ctVal）。
		e.positionValue[p.InstID] = notionalUSDT(p.InstID, p.Size, p.AvgPrice)
	}
	slog.Info("账户同步完成", "positions", len(positions))
	return nil
}

// warmup 拉取历史 K 线喂给策略，丢弃预热期间产生的信号。
// OKX 没数据或拉取失败只 warn，不阻断启动（实时数据来了策略仍能逐步建立状态）。
func (e *Engine) warmup(signals <-chan types.Signal) {
	n := e.strat.WarmupBars()
	if n <= 0 {
		return
	}
	klines, err := e.rest.GetKlines(e.stratCfg.InstID, e.stratCfg.Bar, n)
	if err != nil {
		slog.Warn("预热拉取 K 线失败", "instId", e.stratCfg.InstID, "bar", e.stratCfg.Bar, "err", err)
		return
	}
	// OKX 返回时间倒序（最新在前），反转成正序再喂
	for i, j := 0, len(klines)-1; i < j; i, j = i+1, j-1 {
		klines[i], klines[j] = klines[j], klines[i]
	}
	for _, k := range klines {
		e.strat.OnKline(k)
	}
	dropped := drainSignals(signals)
	slog.Info("预热完成", "instId", e.stratCfg.InstID, "fed", len(klines), "dropped", dropped)
}

// drainSignals 非阻塞排空 channel，返回丢弃数量。
func drainSignals(ch <-chan types.Signal) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// subscribe 根据策略配置订阅必要频道。
func (e *Engine) subscribe() {
	instID := e.stratCfg.InstID
	instType := e.stratCfg.InstType

	// trades / tickers 在 public，candle 在 business
	e.wsPub.Subscribe(okx.WSChannel{Channel: "trades", InstID: instID})
	e.wsBiz.Subscribe(okx.WSChannel{Channel: "candle" + e.stratCfg.Bar, InstID: instID})
	e.wsPriv.Subscribe(okx.WSChannel{Channel: "orders", InstType: string(instType)})
}

func (e *Engine) handleEvent(ev okx.WSEvent) {
	switch {
	case ev.Tick != nil:
		e.mu.Lock()
		e.lastPrice[ev.Tick.InstID] = ev.Tick.Price
		e.mu.Unlock()
		e.strat.OnTick(*ev.Tick)
	case ev.Kline != nil:
		e.strat.OnKline(*ev.Kline)
	case ev.Order != nil:
		e.strat.OnOrderUpdate(*ev.Order)
		// 持仓金额跟踪：成交后更新（粗略估算，精确值靠 GetPositions 同步）
		if ev.Order.Status == types.StatusFilled || ev.Order.Status == types.StatusPartiallyFilled {
			delta := notionalUSDT(ev.Order.InstID, ev.Order.FilledSize, ev.Order.AvgPrice)
			if ev.Order.Side == types.SideSell {
				delta = -delta
			}
			e.mu.Lock()
			e.positionValue[ev.Order.InstID] += delta
			e.mu.Unlock()
		}
	}
}

// handleSignal 处理一个策略信号：风控 → 下单 → 通知。
// 返回 nil 表示 OKX 成功接单（订单 ID 已拿到），非 nil 表示风控拦截 / OKX 拒单 / 网络失败。
//
// Run 模式的调用方可以忽略返回值（已在内部 slog 打错误日志）；
// oneshot 模式用返回值统计成功/失败下单数，给最终决策报告用。
func (e *Engine) handleSignal(sig types.Signal) error {
	e.mu.RLock()
	mark := e.lastPrice[sig.InstID]
	posVal := e.positionValue[sig.InstID]
	e.mu.RUnlock()

	if err := e.risk.Check(sig, mark, posVal); err != nil {
		slog.Warn("信号被风控拦截", "instId", sig.InstID, "side", sig.Side, "err", err)
		e.notifier.Notify(
			fmt.Sprintf("[gotrader] 信号被风控拦截 %s %s", sig.InstID, sig.Side),
			formatSignalNotify(sig, mark, posVal, "BLOCKED", "风控拒绝："+err.Error(), "", ""),
		)
		return fmt.Errorf("blocked by risk: %w", err)
	}

	tdMode := tradeMode(sig.InstType)
	clOID := generateClientOID()

	// SWAP/FUTURES：策略发的 size 是"基础币数量"，OKX 接受的是"张数"
	// 必须用合约面值 ctVal 转换。这里用静态表，未来改成启动时拉 /api/v5/public/instruments
	okxSize := convertToOKXSize(sig.InstID, sig.InstType, sig.Size)

	// posSide 适配账户持仓模式：
	//   - net 模式（OKX 默认单向持仓）：必须不传 posSide，否则 51000 拒单
	//   - long_short 模式（双向持仓）：必须传 long/short
	// 策略层统一发 long/short，由这里按账户实际模式做翻译。
	posSide := sig.PosSide
	if e.positionMode == "net" {
		posSide = types.PosNone
	}

	req := okx.PlaceOrderReq{
		InstID:     sig.InstID,
		TdMode:     tdMode,
		Side:       sig.Side,
		PosSide:    posSide,
		Type:       sig.Type,
		Price:      sig.Price,
		Size:       okxSize,
		ClientOID:  clOID,
		ReduceOnly: sig.ReduceOnly, // 关键：让 OKX 知道这是平仓不是开反向仓
	}
	orderID, err := e.rest.PlaceOrder(req)
	if err != nil {
		slog.Error("下单失败", "instId", sig.InstID, "err", err, "reason", sig.Reason)
		e.notifier.Notify(
			fmt.Sprintf("[gotrader] 下单失败 %s %s", sig.InstID, sig.Side),
			formatSignalNotify(sig, mark, posVal, "FAIL", "下单失败："+err.Error(), clOID, ""),
		)
		return fmt.Errorf("place order: %w", err)
	}
	slog.Info("订单已下", "instId", sig.InstID, "side", sig.Side,
		"strategy_size", sig.Size, "okx_size", okxSize,
		"reduceOnly", sig.ReduceOnly,
		"price", sig.Price, "ordId", orderID, "clOID", clOID, "reason", sig.Reason)
	e.notifier.Notify(
		fmt.Sprintf("[gotrader] 已下单 %s %s %s", sig.InstID, describeAction(sig), sig.Side),
		formatSignalNotify(sig, mark, posVal, "OK", "订单已提交", clOID, orderID),
	)
	return nil
}

// describeAction 把信号翻译成人话："开多" / "平多" / "开空" / "平空"。
// 现货简化为 "买入" / "卖出"。
func describeAction(sig types.Signal) string {
	if sig.InstType == types.InstSpot {
		if sig.Side == types.SideBuy {
			return "买入"
		}
		return "卖出"
	}
	// 合约：reduceOnly 区分开/平
	if sig.ReduceOnly {
		if sig.Side == types.SideSell {
			return "平多"
		}
		return "平空"
	}
	if sig.Side == types.SideBuy {
		return "开多"
	}
	return "开空"
}

// formatSignalNotify 组装邮件正文。
// status 取 OK / FAIL / BLOCKED；clOID/orderID 可为空字符串。
func formatSignalNotify(sig types.Signal, mark, posVal float64,
	status, detail, clOID, orderID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "状态: %s\n", status)
	fmt.Fprintf(&b, "时间: %s\n", time.Now().Format("2006-01-02 15:04:05 -0700"))
	fmt.Fprintf(&b, "标的: %s (%s)\n", sig.InstID, sig.InstType)
	fmt.Fprintf(&b, "动作: %s\n", describeAction(sig))
	fmt.Fprintf(&b, "方向: %s  持仓方向: %s\n", sig.Side, fallback(string(sig.PosSide), "-"))
	fmt.Fprintf(&b, "类型: %s\n", sig.Type)
	fmt.Fprintf(&b, "价格: %s\n", priceStr(sig.Price))
	fmt.Fprintf(&b, "数量(基础币): %g\n", sig.Size)
	fmt.Fprintf(&b, "ReduceOnly: %v\n", sig.ReduceOnly)
	fmt.Fprintf(&b, "杠杆: %d\n", sig.Leverage)
	fmt.Fprintf(&b, "信号原因: %s\n", fallback(sig.Reason, "-"))
	fmt.Fprintf(&b, "\n--- 市场快照 ---\n")
	fmt.Fprintf(&b, "最新价: %s\n", priceStr(mark))
	fmt.Fprintf(&b, "当前持仓估值(USDT): %.2f\n", posVal)
	if clOID != "" {
		fmt.Fprintf(&b, "\n--- 订单 ---\n")
		fmt.Fprintf(&b, "ClientOID: %s\n", clOID)
		if orderID != "" {
			fmt.Fprintf(&b, "OKX OrdID:  %s\n", orderID)
		}
	}
	if detail != "" {
		fmt.Fprintf(&b, "\n备注: %s\n", detail)
	}
	return b.String()
}

func priceStr(p float64) string {
	if p == 0 {
		return "市价"
	}
	return fmt.Sprintf("%g", p)
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// swapCtVal OKX 永续合约面值。1 张 = ctVal 个基础币。
//
// 这是个**静态快照**——OKX 偶尔会改面值（比如 BTC 永续 2024 从 0.01 改过）。
// 生产环境应该在启动时拉 /api/v5/public/instruments 拿到最新值。
//
// 漏配的标的会落到 fallback：直接用 strategy 算的 size（=1 张），可能偏差很大。
var swapCtVal = map[string]float64{
	"BTC-USDT-SWAP":  0.01,
	"ETH-USDT-SWAP":  0.1,
	"SOL-USDT-SWAP":  1.0,
	"DOGE-USDT-SWAP": 1000.0,
	"XRP-USDT-SWAP":  100.0,
	"BNB-USDT-SWAP":  0.01,
	"LTC-USDT-SWAP":  1.0,
	"BCH-USDT-SWAP":  0.1,
	"ADA-USDT-SWAP":  100.0,
	"AVAX-USDT-SWAP": 1.0,
	"MATIC-USDT-SWAP": 10.0,
	"TRX-USDT-SWAP":  1000.0,
}

// notionalUSDT 把 OKX 的"张数"折算成名义 USDT：张数 × 合约面值 × 价格。
//
// 这是持仓/成交估值的唯一入口——bootstrap 同步和成交回报都必须走它，
// 否则"张 × 价格"会漏乘 ctVal，估值虚高 1/ctVal 倍（ETH 是 10 倍），
// 直接污染风控的持仓上限判断。现货 / 未知合约退化 1:1（ct=1）。
func notionalUSDT(instID string, contracts, price float64) float64 {
	ct, ok := swapCtVal[instID]
	if !ok {
		ct = 1
	}
	return contracts * ct * price
}

// contractsToBaseQty 把 OKX 张数折算成基础币数量：张数 × 合约面值。
// 与 notionalUSDT 对称，用于把交易所持仓（张）传给策略（策略 curQty 按基础币记）。
// 现货 / 未知合约退化 1:1（ct=1）。
func contractsToBaseQty(instID string, contracts float64) float64 {
	ct, ok := swapCtVal[instID]
	if !ok {
		ct = 1
	}
	return contracts * ct
}

// convertToOKXSize 把策略层的"基础币数量"转换成 OKX 接受的"张数"。
// 现货不需要转换；合约按 ctVal 折算并向下取整到整数张。
func convertToOKXSize(instID string, instType types.InstType, baseQty float64) float64 {
	if instType != types.InstSwap && instType != types.InstFutures {
		return baseQty
	}
	ct, ok := swapCtVal[instID]
	if !ok {
		slog.Warn("未知合约面值 ctVal，size 不转换（可能严重偏差！）",
			"instId", instID)
		return baseQty
	}
	contracts := baseQty / ct
	// OKX 要求张数为正整数。int() 等价于 floor（对正数）。
	// 宁可仓位略小也不超出预期 size。
	if contracts < 1 {
		return 0 // 量太小，不下单
	}
	return float64(int(contracts))
}

// tradeMode 现货用 cash，合约默认 cross。逐仓需要在策略层显式决定（暂未支持）。
func tradeMode(it types.InstType) string {
	if it == types.InstSpot {
		return "cash"
	}
	return "cross"
}

// generateClientOID 生成 OKX 接受的 clOrdId。
// OKX 要求：1-32 字符，字母数字。
func generateClientOID() string {
	// 时间戳纳秒（保证递增唯一）+ 简单前缀
	ns := time.Now().UnixNano()
	s := fmt.Sprintf("gt%d", ns)
	// 防御：去掉非字母数字
	return strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, s)
}
