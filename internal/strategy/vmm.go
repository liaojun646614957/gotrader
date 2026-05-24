package strategy

import (
	"log/slog"
	"math"

	"github.com/kraus/gotrader/internal/types"
)

// Volatility-Managed Momentum (VMM) 策略。
//
// 学术出处：
//   - Barroso & Santa-Clara (2015) "Momentum has its moments" JFE
//     ★ 原创：用过去 6 个月实现波动率倒数缩放动量收益，几乎翻倍 Sharpe
//   - Springer 2025 (s11408-025-00474-9) "Cryptocurrency momentum has (not) its moments"
//     ★ 加密版：1.6-2.4%/周风险调整收益，但尾部仍未消除
//
// 跟 tsmom 的关键区别（虽然数学上相似，但语义不同）：
//
//	tsmom: 短期动量 (28d) + 同期 vol (28d) + 杠杆放大 → 激进
//	vmm  : 短期方向 (7d)  + 长期 vol (60d) + 无杠杆约束 → 保守
//
// VMM 的核心洞察：
//
//	"动量信号方向"和"仓位大小"应该分开决定：
//	  - 方向：短期看更敏感（论文 1 周）
//	  - 大小：长期看更稳定（论文 6 个月），避免单次大波动毁组合
//
// 这是 Barroso-Santa-Clara "消除特殊情况"的好品味 ——
// 不再让动量信号被"刚好赶上高波动期"的运气主导。
func init() {
	Register("vmm", func() Strategy { return &vmm{} })
}

type vmm struct {
	cfg     Config
	signals chan<- types.Signal

	returnLookback int     // 用过去 N 根 K 线判断动量方向（论文 5-7）
	volLookback    int     // 用过去 N 根 K 线算波动率（论文 ~120 = 6 个月日线）
	rebalanceBars  int     // 再平衡间隔（论文周度 = 7 根日线）
	targetVolAnnual float64 // 年化目标波动率
	barsPerYear     float64 // 年化用
	basePositionUSD float64 // 100% 仓位对应的名义 USD（一般 = 初始资金）
	maxScale        float64 // 上限（VMM 论文用 1.0 = 不超过 base，无杠杆）
	allowShort      bool

	closes []float64

	barsSinceRebalance int
	state              pos
}

func (s *vmm) Init(cfg Config, signals chan<- types.Signal) error {
	s.cfg = cfg
	s.signals = signals
	s.returnLookback = paramInt(cfg.Params, "return_lookback", 7)
	s.volLookback = paramInt(cfg.Params, "vol_lookback", 60)
	s.rebalanceBars = paramInt(cfg.Params, "rebalance_bars", 7)
	s.targetVolAnnual = paramFloat(cfg.Params, "target_vol_annual", 0.40)
	s.barsPerYear = paramFloat(cfg.Params, "bars_per_year", 365)
	s.basePositionUSD = paramFloat(cfg.Params, "base_position_usd", 500)
	s.maxScale = paramFloat(cfg.Params, "max_scale", 1.0) // 默认 1.0 = 无杠杆
	s.allowShort = cfg.InstType == types.InstSwap || cfg.InstType == types.InstFutures

	slog.Info("VMM 策略已初始化",
		"instId", cfg.InstID,
		"return_lookback", s.returnLookback,
		"vol_lookback", s.volLookback,
		"rebalance_bars", s.rebalanceBars,
		"target_vol_annual", s.targetVolAnnual,
		"base_position_usd", s.basePositionUSD,
		"max_scale", s.maxScale,
		"allow_short", s.allowShort,
	)
	return nil
}

// WarmupBars 至少 max(return_lookback, vol_lookback) + buffer。
// 论文 vol_lookback 通常更大（6 个月），主导 warmup 长度。
func (s *vmm) WarmupBars() int {
	rl := paramInt(s.cfg.Params, "return_lookback", 7)
	vl := paramInt(s.cfg.Params, "vol_lookback", 60)
	if vl > rl {
		return vl + 10
	}
	return rl + 10
}

func (s *vmm) OnTick(t types.Tick) {}

func (s *vmm) OnKline(k types.Kline) {
	s.closes = append(s.closes, k.Close)
	// 滚动保留（至少要 vol_lookback + return_lookback 的余量）
	maxLook := s.volLookback
	if s.returnLookback > maxLook {
		maxLook = s.returnLookback
	}
	if len(s.closes) > maxLook*3 {
		s.closes = s.closes[len(s.closes)-maxLook*3:]
	}

	s.barsSinceRebalance++

	if len(s.closes) < s.volLookback+1 {
		return // 数据不足，无法算 vol
	}
	if s.barsSinceRebalance < s.rebalanceBars {
		return
	}
	s.barsSinceRebalance = 0

	s.evaluate(k.Close)
}

// evaluate VMM 核心评估：方向用短期、仓位用长期 vol 缩放。
func (s *vmm) evaluate(curPrice float64) {
	n := len(s.closes)

	// 1) 方向：过去 returnLookback 期收益（"短期信号"）
	startPrice := s.closes[n-1-s.returnLookback]
	if startPrice <= 0 {
		return
	}
	pastReturn := (s.closes[n-1] - startPrice) / startPrice

	// 2) 波动率：过去 volLookback 期实现波动率（"长期稳定"）
	volSeries := s.closes[n-s.volLookback-1 : n]
	vol := stdOfReturns(volSeries)
	annVol := vol * math.Sqrt(s.barsPerYear)
	if annVol < 1e-6 {
		return // 异常数据
	}

	// 3) 方向决定（按 Linus "消除特殊情况"：现货时空头退化为 flat）
	var dir pos
	switch {
	case pastReturn > 0:
		dir = posLong
	case pastReturn < 0 && s.allowShort:
		dir = posShort
	default:
		dir = posFlat
	}

	// 4) 波动率缩放 + 上限约束
	scale := s.targetVolAnnual / annVol
	if scale > s.maxScale {
		scale = s.maxScale
	}
	targetUSD := s.basePositionUSD * scale
	targetQty := targetUSD / curPrice

	slog.Info("VMM 评估",
		"past_return", pastReturn,
		"ann_vol", annVol,
		"scale", scale,
		"target_dir", stateName(dir),
		"target_qty", targetQty,
		"target_usd", targetUSD,
		"cur_price", curPrice,
	)

	s.rebalance(dir, targetQty)
}

// rebalance 调仓到目标方向 + 数量。同 tsmom 的"先平后开"语义。
func (s *vmm) rebalance(dir pos, targetQty float64) {
	if dir == s.state {
		// 同向不动（MVP 不做动态加减仓）
		return
	}

	if s.state != posFlat {
		s.emitClose()
		s.state = posFlat
	}

	if dir == posFlat {
		return
	}

	side := types.SideBuy
	if dir == posShort {
		side = types.SideSell
	}
	s.signals <- types.Signal{
		InstID:     s.cfg.InstID,
		InstType:   s.cfg.InstType,
		Side:       side,
		PosSide:    posSideFor(s.cfg.InstType, side),
		Type:       types.OrderMarket,
		Size:       targetQty,
		ReduceOnly: false,
		Reason:     "vmm 开仓",
	}
	s.state = dir
}

func (s *vmm) emitClose() {
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
		Size:       1, // ReduceOnly=true 时 runner 不看 size 值
		ReduceOnly: true,
		Reason:     "vmm 换向平仓",
	}
}

func (s *vmm) OnOrderUpdate(o types.Order) {
	// 强平同步（止损/手动平仓时）
	if o.Status != types.StatusFilled || !o.ReduceOnly {
		return
	}
	if s.state != posFlat {
		slog.Info("VMM 持仓被外部平掉，同步 state=FLAT",
			"prevState", stateName(s.state),
			"reason", o.Reason,
		)
		s.state = posFlat
	}
}
