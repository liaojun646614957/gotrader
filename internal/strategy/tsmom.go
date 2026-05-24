package strategy

import (
	"log/slog"
	"math"

	"github.com/kraus/gotrader/internal/types"
)

// Time-Series Momentum (TSMOM) 策略。
//
// 学术出处：
//   - Moskowitz, Ooi & Pedersen (2012, JFE) "Time series momentum"
//   - Han, Kang & Ryu (2024, SSRN 4675565) 加密市场实证
//     最佳参数 (lookback=28, holding=5) 加密上 Sharpe 1.51
//   - Springer 2025 (s11408-025-00474-9) 强调"波动率缩放"对加密尤其重要
//
// ⚠️ base_position_usd 在回测 vs 实盘的语义不同 ⚠️
//
//	回测（backtest.runner）：
//	  - 回测引擎不模拟 OKX 杠杆，size 直接 = 名义价值 / 价格
//	  - 若想测 3x 杠杆效果，配 base_position_usd = init_equity * 3
//
//	实盘（engine.go）：
//	  - OKX 杠杆由 strategy.leverage 字段在账户上设置（自动放大保证金 → 名义仓位）
//	  - base_position_usd 应 = 你的本金 USDT 数额（OKX 自动放大）
//	  - 例：500 USDT 本金 + leverage=3 → 配 base=500，OKX 实际开 1500 USDT 名义仓
//	  - 配错（base=1500）会导致名义仓位 4500 USDT，3x 杠杆需要 1500 USDT 保证金，
//	    超过你 500 USDT 本金 → 立即被拒或爆仓
//
// 核心逻辑（消除特殊情况）：
//
//	每 holding_bars 根 K 线评估一次：
//	  past_return = (close - close[t-lookback]) / close[t-lookback]
//	  vol = std(daily_returns[t-lookback : t])
//	  ann_vol = vol * sqrt(bars_per_year)
//	  direction = sign(past_return)              ← 多/空/平
//	  scale = target_vol / ann_vol               ← 波动率缩放，高波动时缩仓
//	  target_qty = base_position_usd * scale / close
//	  调仓到 target_qty
//
// 跟缠论的本质区别：
//   - 缠论：试图识别"结构"，5m 上结构 = 噪声
//   - TSMOM：只看"过去 N 天涨还是跌"，简单暴力但有学术证据
//   - TSMOM 的"波动率缩放"是关键，没有它就退化成 chan_bs 那种亏法
func init() {
	Register("tsmom", func() Strategy { return &tsmom{} })
}

type tsmom struct {
	cfg     Config
	signals chan<- types.Signal

	lookbackBars    int     // 回看 K 线数（论文日线最优 28）
	holdingBars     int     // 持有期/再平衡间隔（论文 5）
	targetVolAnnual float64 // 年化目标波动率（0.4 = 40%）
	barsPerYear     float64 // 一年有多少根 K 线（5m=105120, 1H=8760, 1D=365）
	basePositionUSD float64 // 基准名义仓位（USDT），实际仓位会按 vol scale 上下浮动
	maxScale        float64 // vol scale 的上限（防止低波动期仓位失控）
	allowShort      bool

	closes []float64 // 滚动保存收盘价（用于算 return 和 vol）

	// 距上次评估的 K 线数。到达 holdingBars 就重新评估。
	barsSinceEval int

	// 当前持仓状态。同 chan_bs 的 pos 枚举。
	state pos
}

func (s *tsmom) Init(cfg Config, signals chan<- types.Signal) error {
	s.cfg = cfg
	s.signals = signals
	s.lookbackBars = paramInt(cfg.Params, "lookback_bars", 28)
	s.holdingBars = paramInt(cfg.Params, "holding_bars", 5)
	s.targetVolAnnual = paramFloat(cfg.Params, "target_vol_annual", 0.40)
	s.barsPerYear = paramFloat(cfg.Params, "bars_per_year", 365)
	s.basePositionUSD = paramFloat(cfg.Params, "base_position_usd", 1000)
	s.maxScale = paramFloat(cfg.Params, "max_scale", 3.0)
	s.allowShort = cfg.InstType == types.InstSwap || cfg.InstType == types.InstFutures

	slog.Info("TSMOM 策略已初始化",
		"instId", cfg.InstID,
		"lookback_bars", s.lookbackBars,
		"holding_bars", s.holdingBars,
		"target_vol_annual", s.targetVolAnnual,
		"bars_per_year", s.barsPerYear,
		"base_position_usd", s.basePositionUSD,
		"allow_short", s.allowShort,
	)
	return nil
}

// WarmupBars 至少 lookback + 一些 buffer，保证第一次评估时数据充足。
func (s *tsmom) WarmupBars() int {
	lb := paramInt(s.cfg.Params, "lookback_bars", 28)
	return lb + 10
}

func (s *tsmom) OnTick(t types.Tick) {}

func (s *tsmom) OnKline(k types.Kline) {
	s.appendClose(k.Close)
	s.barsSinceEval++

	if !s.hasEnoughData() {
		return
	}
	// 持有期未到
	if s.barsSinceEval < s.holdingBars {
		return
	}
	s.barsSinceEval = 0
	s.evaluate(k.Close)
}

// ForceEvaluate 实现 strategy.ForceEvaluator。
//
// 用途：oneshot / cron 模式下由外部触发频率代替 holding_bars 内部计数。
// 调用者保证"现在该做决策了"，本方法只负责：喂数据 + 重置内部计数 + 评估。
//
// 与 OnKline 的差异：
//   - OnKline 在持有期内会直接 return，决策 K 可能错过评估
//   - ForceEvaluate 无视持有期计数，只要数据够就一定评估
func (s *tsmom) ForceEvaluate(k types.Kline) {
	s.appendClose(k.Close)
	s.barsSinceEval = 0 // 重置，下次 OnKline 重新累计

	if !s.hasEnoughData() {
		slog.Warn("ForceEvaluate 数据不足，跳过本次评估",
			"have", len(s.closes), "need", s.lookbackBars+1)
		return
	}
	s.evaluate(k.Close)
}

// appendClose 把一根 K 线收盘价加入滚动缓存。
// lookback * 4 的窗口对 28 日策略 = 112，绰绰有余。
func (s *tsmom) appendClose(c float64) {
	s.closes = append(s.closes, c)
	if len(s.closes) > s.lookbackBars*4 {
		s.closes = s.closes[len(s.closes)-s.lookbackBars*4:]
	}
}

// hasEnoughData 评估所需数据是否就位：要算 lookback 期收益率至少需要 lookback+1 个 close。
func (s *tsmom) hasEnoughData() bool {
	return len(s.closes) >= s.lookbackBars+1
}

// evaluate 跑一次 TSMOM 评估并调仓。
func (s *tsmom) evaluate(curPrice float64) {
	n := len(s.closes)

	// 1) 过去 lookback 期收益率
	startPrice := s.closes[n-1-s.lookbackBars]
	pastReturn := (s.closes[n-1] - startPrice) / startPrice

	// 2) 实现波动率（按 lookback 期内的对数收益率算更稳，但简单收益率也够用）
	vol := stdOfReturns(s.closes[n-s.lookbackBars-1 : n])
	annVol := vol * math.Sqrt(s.barsPerYear)
	if annVol < 1e-6 {
		// 完全没波动的数据异常，跳过
		return
	}

	// 3) 决定方向
	var dir pos
	switch {
	case pastReturn > 0:
		dir = posLong
	case pastReturn < 0 && s.allowShort:
		dir = posShort
	default:
		// 现货模式 + 负收益 → 持币不动（空仓）
		dir = posFlat
	}

	// 4) 波动率缩放：高波动期 → 缩仓，低波动期 → 加仓
	scale := s.targetVolAnnual / annVol
	if scale > s.maxScale {
		scale = s.maxScale
	}
	targetUSD := s.basePositionUSD * scale
	targetQty := targetUSD / curPrice

	slog.Info("TSMOM 评估",
		"past_return", pastReturn,
		"ann_vol", annVol,
		"scale", scale,
		"target_dir", stateName(dir),
		"target_qty", targetQty,
		"cur_price", curPrice,
	)

	s.rebalance(dir, targetQty)
}

// rebalance 把当前持仓调整到目标方向 + 目标数量。
//
// 状态机消除特殊情况（FLAT/LONG/SHORT 同一处理路径）：
//   - 当前 != 目标方向：先平再开
//   - 目标 = FLAT：仅平仓
//   - 当前 == 目标方向 + size 变化：MVP 不做调仓（避免频繁手续费），等下次周期
func (s *tsmom) rebalance(dir pos, targetQty float64) {
	if dir == s.state {
		// 同方向：MVP 不做"动态加减仓"，等下次评估周期
		return
	}

	// 先平（如果有仓）
	if s.state != posFlat {
		s.emitClose()
		s.state = posFlat
	}

	if dir == posFlat {
		return
	}

	// 开新仓
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
		Reason:     "tsmom 开仓",
	}
	s.state = dir
}

// emitClose 发反向 ReduceOnly 信号平掉当前仓。
func (s *tsmom) emitClose() {
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
		// Size=0 让 runner 用当前持仓量平掉。但我们这里其实知道精确量，
		// 简化：用一个足够大的 size，runner 的 ReduceOnly 逻辑会把目标限到 0。
		// 实际 runner.targetQty 看到 ReduceOnly=true 直接返回 0，所以 Size 字段无所谓。
		Size:       1,
		ReduceOnly: true,
		Reason:     "tsmom 换向平仓",
	}
}

func (s *tsmom) OnOrderUpdate(o types.Order) {
	// 与 chan_bs 同样的同步逻辑：被外部强平（如止损）时 state 重置
	if o.Status != types.StatusFilled || !o.ReduceOnly {
		return
	}
	if s.state != posFlat {
		slog.Info("TSMOM 持仓被外部平掉，同步 state=FLAT",
			"prevState", stateName(s.state),
			"reason", o.Reason,
		)
		s.state = posFlat
	}
}

// stdOfReturns 计算"相邻 K 线收益率序列"的样本标准差。
//
// 输入 prices 长度 n，输出长度 n-1 的收益率的标准差。
// 用样本方差（除以 n-1）而不是总体方差（除以 n），与统计软件默认一致。
func stdOfReturns(prices []float64) float64 {
	if len(prices) < 2 {
		return 0
	}
	rets := make([]float64, len(prices)-1)
	var sum float64
	for i := 1; i < len(prices); i++ {
		if prices[i-1] == 0 {
			rets[i-1] = 0
			continue
		}
		r := (prices[i] - prices[i-1]) / prices[i-1]
		rets[i-1] = r
		sum += r
	}
	mean := sum / float64(len(rets))
	var sumSq float64
	for _, r := range rets {
		d := r - mean
		sumSq += d * d
	}
	if len(rets) < 2 {
		return 0
	}
	variance := sumSq / float64(len(rets)-1)
	return math.Sqrt(variance)
}
