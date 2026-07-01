package strategy

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// 用现货模式（不允许做空）验证：
//   - 上涨段 → emit buy
//   - 下跌段 → 不 emit（空仓持币）
func TestTSMOM_SpotUptrend(t *testing.T) {
	s := &tsmom{}
	signals := make(chan types.Signal, 32)
	cfg := Config{
		InstID:   "BTC-USDT",
		InstType: types.InstSpot,
		Params: map[string]interface{}{
			"lookback_bars":     5,
			"holding_bars":      1,
			"target_vol_annual": 1.0, // 大点避免被缩到 0
			"bars_per_year":     365.0,
			"base_position_usd": 100.0,
			"max_scale":         100.0,
		},
	}
	if err := s.Init(cfg, signals); err != nil {
		t.Fatal(err)
	}

	// 单调上涨 10 根
	for i := 0; i < 10; i++ {
		s.OnKline(types.Kline{Close: float64(100 + i*5)})
	}

	// 应该 emit 过 buy
	var buys, sells int
	close(signals)
	for sig := range signals {
		if sig.Side == types.SideBuy && !sig.ReduceOnly {
			buys++
		}
		if sig.Side == types.SideSell {
			sells++
		}
	}
	if buys == 0 {
		t.Error("上涨段应至少 emit 一次 Buy")
	}
	// 现货模式不应有任何 sell（除非平仓，但 state 从未变成 short）
	if sells > 0 {
		t.Errorf("现货模式不应 emit Sell, got %d", sells)
	}
}

// 合约模式：上涨开多、下跌开空。
func TestTSMOM_SwapBothDirections(t *testing.T) {
	s := &tsmom{}
	signals := make(chan types.Signal, 64)
	cfg := Config{
		InstID:   "BTC-USDT-SWAP",
		InstType: types.InstSwap,
		Params: map[string]interface{}{
			"lookback_bars":     5,
			"holding_bars":      1,
			"target_vol_annual": 1.0,
			"bars_per_year":     365.0,
			"base_position_usd": 100.0,
			"max_scale":         100.0,
		},
	}
	s.Init(cfg, signals)

	// 上涨 → 下跌
	for i := 0; i < 8; i++ {
		s.OnKline(types.Kline{Close: float64(100 + i*5)})
	}
	for i := 0; i < 10; i++ {
		s.OnKline(types.Kline{Close: float64(140 - i*5)})
	}

	close(signals)
	var openBuy, openSell, closeBuy, closeSell int
	for sig := range signals {
		switch {
		case sig.Side == types.SideBuy && !sig.ReduceOnly:
			openBuy++
		case sig.Side == types.SideSell && !sig.ReduceOnly:
			openSell++
		case sig.Side == types.SideBuy && sig.ReduceOnly:
			closeBuy++ // 平空
		case sig.Side == types.SideSell && sig.ReduceOnly:
			closeSell++ // 平多
		}
	}
	if openBuy == 0 {
		t.Error("应该至少开过一次多仓")
	}
	if openSell == 0 {
		t.Error("应该至少开过一次空仓")
	}
	if closeSell == 0 {
		t.Error("从多翻空过程中应该有平多动作")
	}
}

// 波动率缩放：高波动期 size 应比低波动期 size 小。
func TestTSMOM_VolatilityScaling(t *testing.T) {
	// 低波动场景
	sLow := &tsmom{}
	sigLow := make(chan types.Signal, 32)
	sLow.Init(Config{
		InstID:   "T",
		InstType: types.InstSpot,
		Params: map[string]interface{}{
			"lookback_bars":     5,
			"holding_bars":      1,
			"target_vol_annual": 0.5,
			"bars_per_year":     365.0,
			"base_position_usd": 1000.0,
			"max_scale":         100.0,
		},
	}, sigLow)
	for i := 0; i < 10; i++ {
		// 上涨但波动很小：每步 +0.1
		sLow.OnKline(types.Kline{Close: 100 + float64(i)*0.1})
	}

	// 高波动场景
	sHi := &tsmom{}
	sigHi := make(chan types.Signal, 32)
	sHi.Init(Config{
		InstID:   "T",
		InstType: types.InstSpot,
		Params: map[string]interface{}{
			"lookback_bars":     5,
			"holding_bars":      1,
			"target_vol_annual": 0.5,
			"bars_per_year":     365.0,
			"base_position_usd": 1000.0,
			"max_scale":         100.0,
		},
	}, sigHi)
	// 上涨但波动很大：100, 95, 105, 100, 110, 105, 115, 110, 120, 115
	hiPrices := []float64{100, 95, 105, 100, 110, 105, 115, 110, 120, 115}
	for _, p := range hiPrices {
		sHi.OnKline(types.Kline{Close: p})
	}

	close(sigLow)
	close(sigHi)
	var lowSize, hiSize float64
	for sig := range sigLow {
		if sig.Side == types.SideBuy && !sig.ReduceOnly {
			lowSize = sig.Size
			break
		}
	}
	for sig := range sigHi {
		if sig.Side == types.SideBuy && !sig.ReduceOnly {
			hiSize = sig.Size
			break
		}
	}
	if lowSize == 0 || hiSize == 0 {
		t.Skipf("数据没产生买入信号, lowSize=%v hiSize=%v", lowSize, hiSize)
	}
	if hiSize >= lowSize {
		t.Errorf("高波动应缩仓: hiSize=%v 应 < lowSize=%v", hiSize, lowSize)
	}
	t.Logf("低波动 size=%.4f, 高波动 size=%.4f, 比例 %.2fx",
		lowSize, hiSize, lowSize/hiSize)
}

// genSeries 生成"趋势 + 锯齿噪声"的收盘价序列。
// drift 控制每步上涨幅度，amp 控制相邻摆动（即波动率）。
func genSeries(start, drift, amp float64, n int) []float64 {
	out := make([]float64, n)
	p := start
	for i := 0; i < n; i++ {
		p *= 1 + drift
		jitter := amp
		if i%2 == 1 {
			jitter = -amp
		}
		out[i] = p * (1 + jitter)
	}
	return out
}

func feedCloses(s *tsmom, closes []float64) {
	for _, c := range closes {
		s.OnKline(types.Kline{InstID: "ETH-USDT-SWAP", Open: c, High: c, Low: c, Close: c})
	}
}

func splitSignals(sigs []types.Signal) (closes, opens []types.Signal) {
	for _, sg := range sigs {
		if sg.ReduceOnly {
			closes = append(closes, sg)
		} else {
			opens = append(opens, sg)
		}
	}
	return
}

func drainSignalsTest(ch chan types.Signal) []types.Signal {
	var out []types.Signal
	for {
		select {
		case sg := <-ch:
			out = append(out, sg)
		default:
			return out
		}
	}
}

func newTSMOMForResize(t *testing.T) (*tsmom, chan types.Signal) {
	t.Helper()
	s := &tsmom{}
	sigs := make(chan types.Signal, 256)
	cfg := Config{
		InstID:   "ETH-USDT-SWAP",
		InstType: types.InstSwap,
		Params: map[string]interface{}{
			"lookback_bars":     10,
			"holding_bars":      1,
			"target_vol_annual": 0.40,
			"bars_per_year":     365.0,
			"base_position_usd": 1000.0,
			"max_scale":         3.0,
			"resize_threshold":  0.25,
		},
	}
	if err := s.Init(cfg, sigs); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s, sigs
}

// TestTSMOM_VolScalingResizesSameDirection 覆盖此前没人测到的核心 bug：
// 方向不变、但波动率升高导致目标仓位显著缩小时，策略必须真的减仓。
//
// 旧实现里 rebalance 对同方向直接 return，vol scaling 名存实亡——
// 这个测试在旧代码上会失败（阶段2 收不到任何信号）。
func TestTSMOM_VolScalingResizesSameDirection(t *testing.T) {
	s, sigs := newTSMOMForResize(t)

	// 阶段1：低波动缓涨 → 多头 + 高 scale(撞 cap=3) → 大仓位
	feedCloses(s, genSeries(100, 0.003, 0.002, 30))
	openSize1, ok := lastOpenSize(drainSignalsTest(sigs))
	if !ok {
		t.Fatalf("阶段1 应开多仓，却没有开仓信号")
	}

	// 阶段2：高波动继续上涨 → 仍多头，但 annVol 飙升 → scale 变小 → 目标仓位显著缩小
	feedCloses(s, genSeries(s.closes[len(s.closes)-1], 0.015, 0.04, 30))
	closes, opens := splitSignals(drainSignalsTest(sigs))

	if len(closes) == 0 || len(opens) == 0 {
		t.Fatalf("波动率升高应触发同方向减仓(先平后开)，实际 closes=%d opens=%d。"+
			"vol scaling 没生效。", len(closes), len(opens))
	}
	openSize2 := opens[len(opens)-1].Size
	if openSize2 >= openSize1 {
		t.Fatalf("高波动应缩仓：openSize2=%.4f 应 < openSize1=%.4f", openSize2, openSize1)
	}
	// 平仓信号的 Size 应等于平仓前的持仓量（curQty），不是写死的 1
	if closes[0].Size <= 0 {
		t.Errorf("平仓 Size 应为正的持仓量, got %.4f", closes[0].Size)
	}
	t.Logf("vol scaling 生效：低波动仓位 %.4f → 高波动仓位 %.4f", openSize1, openSize2)
}

// TestTSMOM_NoResizeOnSmallDrift 回归保护：同方向且 size 漂移很小时不应频繁调仓，
// 否则手续费会被无意义的微调吃掉。
func TestTSMOM_NoResizeOnSmallDrift(t *testing.T) {
	s, sigs := newTSMOMForResize(t)

	feedCloses(s, genSeries(100, 0.003, 0.002, 30)) // 低波动建多仓(撞 cap)
	_ = drainSignalsTest(sigs)

	// 再喂一段几乎同样的低波动 → scale 仍撞 cap，目标 size 几乎不变 → 不应有新信号
	feedCloses(s, genSeries(s.closes[len(s.closes)-1], 0.003, 0.002, 12))
	after := drainSignalsTest(sigs)
	if len(after) != 0 {
		t.Fatalf("小漂移不应调仓，却收到 %d 个信号: %+v", len(after), after)
	}
}

func lastOpenSize(sigs []types.Signal) (float64, bool) {
	_, opens := splitSignals(sigs)
	if len(opens) == 0 {
		return 0, false
	}
	return opens[len(opens)-1].Size, true
}
