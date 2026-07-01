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
//
// ⚠️ 已停用（2026-06-24）⚠️
//
//	ETH-USDT-SWAP 1D / 770 根回测（2024-05 ~ 2026-06）实测：
//	  Sharpe -0.31 / 胜率 24.6% / 净收益 -14%（PF 0.82 < 1）
//	方向预测系统性反向（76% 时候方向错），属于本质无 alpha 的策略，
//	vol-scaling 修复也救不活（修复后 Sharpe -0.34，仅多花手续费）。
//	代码与测试保留，但不再注册——config 选 "vmm" 会得到 "unknown strategy"。
//	将来若想重新验证（换参数/标的/时间段），取消下面 Register 的注释即可。
func init() {
	// Register("vmm", func() Strategy { return &vmm{} })
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
	// resizeThreshold 同方向调仓阈值：目标仓位相对当前持仓漂移超过此比例才调仓。
	// 没有它，方向不变时波动率缩放永远不落地——VMM 的核心卖点就是 vol scaling，
	// 不调仓等于把策略阉割成普通动量。
	resizeThreshold float64
	allowShort      bool

	closes []float64

	barsSinceRebalance int
	state              pos

	// 当前持仓的基础币数量（策略视角）。用于同方向 vol-scaling 调仓时算漂移。
	curQty float64
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
	s.resizeThreshold = paramFloat(cfg.Params, "resize_threshold", 0.25)
	s.allowShort = cfg.InstType == types.InstSwap || cfg.InstType == types.InstFutures

	slog.Info("VMM 策略已初始化",
		"instId", cfg.InstID,
		"return_lookback", s.returnLookback,
		"vol_lookback", s.volLookback,
		"rebalance_bars", s.rebalanceBars,
		"target_vol_annual", s.targetVolAnnual,
		"base_position_usd", s.basePositionUSD,
		"max_scale", s.maxScale,
		"resize_threshold", s.resizeThreshold,
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

// rebalance 调仓到目标方向 + 数量。
//
// 统一路径（消除特殊情况）：是否动作只有一处判断，动作只有"先平后开"一条路。
//   - 方向变了                              → 平掉旧仓 + 开出新仓
//   - 同方向但目标 size 漂移超 resizeThreshold → 平掉旧仓 + 按新 size 重开（vol scaling 在此落地）
//   - 同方向且 size 漂移很小                  → 什么都不做，省手续费
//   - 目标 = FLAT                           → 只平仓
//
// ⚠️ 为什么"先全平再重开"而不是"增量加减仓"：见 tsmom.rebalance 的详细说明。
//
//	简言之：回测 runner 把 Signal.Size 当"目标绝对持仓"，实盘 engine 当"下单增量"，
//	只有"从空仓开到 target"和"ReduceOnly 平到 0"两个动作在两边语义一致。
//	先平后开全程只用这两个动作，避免回测对、实盘错。
func (s *vmm) rebalance(dir pos, targetQty float64) {
	sameDir := dir == s.state
	if sameDir && (dir == posFlat || !s.sizeDriftExceeds(targetQty)) {
		return // 方向没变且仓位无需调整
	}
	s.closeIfAny()
	if dir != posFlat {
		s.open(dir, targetQty)
	}
}

// sizeDriftExceeds 判断目标仓位相对当前持仓的漂移是否大到需要调仓。
// curQty<=0 时返回 true（保证总能开出仓位；正常同方向流程里 curQty 必 >0）。
func (s *vmm) sizeDriftExceeds(targetQty float64) bool {
	if s.curQty <= 0 {
		return true
	}
	return math.Abs(targetQty-s.curQty)/s.curQty > s.resizeThreshold
}

// closeIfAny 平掉当前仓位（如有），并把 state/curQty 重置为空仓。
func (s *vmm) closeIfAny() {
	if s.state == posFlat {
		return
	}
	s.emitClose()
	s.state = posFlat
	s.curQty = 0
}

// open 从空仓开出 targetQty 的新仓，记录 state/curQty。调用前必须已空仓。
func (s *vmm) open(dir pos, targetQty float64) {
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
	s.curQty = targetQty
}

// emitClose 发反向 ReduceOnly 信号平掉当前仓。Size 用 curQty（见 tsmom.emitClose 说明）：
// 回测 runner 无视 Size 直接平到 0；实盘 OKX 按此量 ReduceOnly 平仓，保证不反向。
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
		Size:       s.curQty,
		ReduceOnly: true,
		Reason:     "vmm 平仓",
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
		s.curQty = 0
	}
}
