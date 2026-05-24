package strategy

import (
	"log/slog"

	"github.com/kraus/gotrader/internal/chanlun"
	"github.com/kraus/gotrader/internal/types"
)

// 缠论第 1/2 类买卖点策略 —— 多空双向版。
//
// 状态机（消除特殊情况：FLAT / LONG / SHORT 同一套路径）：
//
//	FLAT + 买点 → pending 等二次确认 → 开多 → LONG
//	FLAT + 卖点 → pending 等二次确认 → 开空 → SHORT （仅 SWAP）
//	LONG + 卖点 → 立即平多 → FLAT，并把卖点入 pending 等开空确认（仅 SWAP）
//	SHORT + 买点 → 立即平空 → FLAT，并把买点入 pending 等开多确认
//	LONG + 买点 / SHORT + 卖点 → 忽略（同向不加仓）
//
// SPOT 模式下不开空，卖点的"反向开仓"那一步会被自动跳过。

func init() {
	Register("chan_bs", func() Strategy { return &chanBS{} })
}

// pos 持仓状态（语义清晰，没有 bool 的歧义）
type pos int8

const (
	posFlat  pos = 0
	posLong  pos = 1
	posShort pos = -1
)

type chanBS struct {
	cfg     Config
	signals chan<- types.Signal

	eng        *chanlun.Engine
	size       float64
	confirm    int  // 二次确认所需 K 线数
	allowShort bool // SWAP/FUTURES = true

	state        pos              // 当前持仓
	pending      *chanlun.BSPoint // 等二次确认的买卖点
	confirmCount int
}

func (s *chanBS) Init(cfg Config, signals chan<- types.Signal) error {
	s.cfg = cfg
	s.signals = signals
	s.size = paramFloat(cfg.Params, "size", 0.001)
	s.confirm = paramInt(cfg.Params, "confirm_bars", 2)
	s.allowShort = cfg.InstType == types.InstSwap || cfg.InstType == types.InstFutures

	macdFast := paramInt(cfg.Params, "macd_fast", 12)
	macdSlow := paramInt(cfg.Params, "macd_slow", 26)
	macdSignal := paramInt(cfg.Params, "macd_signal", 9)
	minBiMKs := paramInt(cfg.Params, "min_bi_mks", 5)
	fxConfirm := paramInt(cfg.Params, "fractal_right_confirm", 1)

	s.eng = chanlun.NewEngine(chanlun.Config{
		MACDFast:            macdFast,
		MACDSlow:            macdSlow,
		MACDSignal:          macdSignal,
		FractalRightConfirm: fxConfirm,
		MinBiMKs:            minBiMKs,
	})

	slog.Info("缠论策略已初始化",
		"instId", cfg.InstID,
		"size", s.size,
		"confirm_bars", s.confirm,
		"allow_short", s.allowShort,
		"macd", []int{macdFast, macdSlow, macdSignal},
		"min_bi_mks", minBiMKs,
	)
	return nil
}

// WarmupBars 取 macd_slow * 8 兜底 200 根，保证 MACD 收敛 + 至少有几笔。
func (s *chanBS) WarmupBars() int {
	macdSlow := paramInt(s.cfg.Params, "macd_slow", 26)
	n := macdSlow * 8
	if n < 200 {
		n = 200
	}
	return n
}

func (s *chanBS) OnTick(t types.Tick) {}

func (s *chanBS) OnKline(k types.Kline) {
	newPoints := s.eng.Feed(k)
	for _, bs := range newPoints {
		s.onBSPoint(bs)
	}
	if s.pending != nil {
		s.advanceConfirm(k)
	}
}

// onBSPoint 处理新的买卖点。
//
// 核心思路：买卖点决定"目标方向"，状态决定"先平后开还是直接开"。
func (s *chanBS) onBSPoint(bs chanlun.BSPoint) {
	isBuy := bs.Kind == 1 || bs.Kind == 2

	slog.Info("缠论买卖点出现",
		"kind", bsKindName(bs.Kind),
		"price", bs.Price,
		"state", stateName(s.state),
		"reason", bs.Reason,
	)

	if isBuy {
		switch s.state {
		case posLong:
			return // 同向忽略
		case posShort:
			// 反向：立即平空（不等二次确认）
			s.emitClose("反向买点立即平空: " + bs.Reason)
			s.state = posFlat
			// 接着尝试开多 → 走二次确认
			s.queue(bs)
		case posFlat:
			s.queue(bs)
		}
		return
	}

	// 卖点
	switch s.state {
	case posShort:
		return // 同向忽略
	case posLong:
		// 立即平多
		s.emitClose("反向卖点立即平多: " + bs.Reason)
		s.state = posFlat
		// 若允许开空 → 走二次确认；否则到此结束
		if s.allowShort {
			s.queue(bs)
		}
	case posFlat:
		if !s.allowShort {
			return // 现货模式不开空
		}
		s.queue(bs)
	}
}

// queue 把买卖点设为 pending，重置确认计数。
func (s *chanBS) queue(bs chanlun.BSPoint) {
	bsCopy := bs
	s.pending = &bsCopy
	s.confirmCount = 0
}

// advanceConfirm 推进二次确认。
//
// 买点：要求未跌破触发低点，且 close > 触发价 才计数+1。
// 卖点：要求未突破触发高点，且 close < 触发价 才计数+1。
// 到达 confirm_bars → 真正开仓。
//
// 期间被反向价格击穿 → pending 作废（避免假信号成交）。
func (s *chanBS) advanceConfirm(k types.Kline) {
	p := s.pending
	switch p.Kind {
	case 1, 2:
		if k.Low < p.Price {
			slog.Info("缠论买点确认失败：跌破触发低点", "trigger", p.Price, "low", k.Low)
			s.pending = nil
			s.confirmCount = 0
			return
		}
		if k.Close > p.Price {
			s.confirmCount++
		}
		if s.confirmCount >= s.confirm {
			s.emitOpen(types.SideBuy, "chan_bs "+bsKindName(p.Kind)+": "+p.Reason)
			s.state = posLong
			s.pending = nil
			s.confirmCount = 0
		}
	case -1, -2:
		if k.High > p.Price {
			slog.Info("缠论卖点确认失败：突破触发高点", "trigger", p.Price, "high", k.High)
			s.pending = nil
			s.confirmCount = 0
			return
		}
		if k.Close < p.Price {
			s.confirmCount++
		}
		if s.confirmCount >= s.confirm {
			s.emitOpen(types.SideSell, "chan_bs "+bsKindName(p.Kind)+": "+p.Reason)
			s.state = posShort
			s.pending = nil
			s.confirmCount = 0
		}
	}
}

// emitOpen 发开仓信号（ReduceOnly=false）。
func (s *chanBS) emitOpen(side types.Side, reason string) {
	s.signals <- types.Signal{
		InstID:     s.cfg.InstID,
		InstType:   s.cfg.InstType,
		Side:       side,
		PosSide:    posSideFor(s.cfg.InstType, side),
		Type:       types.OrderMarket,
		Size:       s.size,
		ReduceOnly: false,
		Reason:     reason,
	}
}

// emitClose 发平仓信号（ReduceOnly=true）。
// Side 取当前持仓方向的反向：持多平多 = Sell；持空平空 = Buy。
func (s *chanBS) emitClose(reason string) {
	var side types.Side
	var ps types.PosSide
	switch s.state {
	case posLong:
		side = types.SideSell
		ps = posSideClose(s.cfg.InstType, posLong)
	case posShort:
		side = types.SideBuy
		ps = posSideClose(s.cfg.InstType, posShort)
	default:
		return
	}
	s.signals <- types.Signal{
		InstID:     s.cfg.InstID,
		InstType:   s.cfg.InstType,
		Side:       side,
		PosSide:    ps,
		Type:       types.OrderMarket,
		Size:       s.size,
		ReduceOnly: true,
		Reason:     reason,
	}
}

// OnOrderUpdate 同步策略 state 与实际持仓。
//
// 关键作用：当 runner 因止损/最大回撤强平持仓时，策略必须感知到，
// 否则 state 会留在 LONG/SHORT，下次反向信号被错误地当成"反转"而不是"开新仓"。
//
// 触发 state 重置的条件：
//   - 成交完成 (Status=Filled)
//   - 且 ReduceOnly=true（这是平仓订单的特征，无论来自策略自身的 emitClose
//     还是 runner 的 stop_loss）
//
// 开仓订单（ReduceOnly=false）的 state 由 advanceConfirm 在 emit 那一刻乐观设置，
// OnOrderUpdate 不需要再处理（回测里 fillOne 立即回调 OnOrderUpdate，
// 此时 state 已是新值，二次同步是 no-op）。
func (s *chanBS) OnOrderUpdate(o types.Order) {
	slog.Debug("订单更新", "ordId", o.OrderID, "status", o.Status,
		"reduceOnly", o.ReduceOnly, "reason", o.Reason)

	if o.Status != types.StatusFilled || !o.ReduceOnly {
		return
	}
	// 强平：如果是 runner 触发的（reason=stop_loss 等非策略发起的平仓），
	// 策略层 state 尚未更新，这里强制同步。
	// 如果是策略自己的 emitClose 发起的，state 已经在 onBSPoint 里 set 为 posFlat，
	// 这里再次 set 也无害。
	if s.state != posFlat {
		slog.Info("持仓被外部平掉（含止损/手动撤单），同步 state=FLAT",
			"prevState", stateName(s.state),
			"reason", o.Reason,
		)
		s.state = posFlat
		// 清掉可能存在的 pending 信号（强平后再走老的待确认毫无意义）
		s.pending = nil
		s.confirmCount = 0
	}
}

// posSideFor 开仓时填的 PosSide。
//
// OKX 单向持仓模式下 PosSide 留 ""；双向持仓模式下：
//   - 开多 = long, 开空 = short
//
// MVP 简化：现货返回空，合约按 side 填。如果你的账户是单向持仓，
// OKX 会忽略这个字段，所以填错也不影响。
func posSideFor(it types.InstType, side types.Side) types.PosSide {
	if it == types.InstSpot {
		return types.PosNone
	}
	if side == types.SideBuy {
		return types.PosLong
	}
	return types.PosShort
}

// posSideClose 平仓时填的 PosSide。和被平仓的持仓方向一致。
func posSideClose(it types.InstType, p pos) types.PosSide {
	if it == types.InstSpot {
		return types.PosNone
	}
	if p == posLong {
		return types.PosLong
	}
	return types.PosShort
}

func bsKindName(k int) string {
	switch k {
	case 1:
		return "1类买"
	case -1:
		return "1类卖"
	case 2:
		return "2类买"
	case -2:
		return "2类卖"
	}
	return "?"
}

func stateName(p pos) string {
	switch p {
	case posLong:
		return "LONG"
	case posShort:
		return "SHORT"
	}
	return "FLAT"
}
