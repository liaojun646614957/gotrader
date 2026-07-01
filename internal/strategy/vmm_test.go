package strategy

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// 上涨趋势 + 现货：应该开多。
func TestVMM_SpotUptrend(t *testing.T) {
	s := &vmm{}
	signals := make(chan types.Signal, 64)
	cfg := Config{
		InstID:   "BTC-USDT",
		InstType: types.InstSpot,
		Params: map[string]interface{}{
			"return_lookback":   3,
			"vol_lookback":      10,
			"rebalance_bars":    1,
			"target_vol_annual": 1.0,
			"bars_per_year":     365.0,
			"base_position_usd": 500.0,
			"max_scale":         5.0, // 测试用宽松一点
		},
	}
	if err := s.Init(cfg, signals); err != nil {
		t.Fatal(err)
	}

	// 单调上涨 20 根（足够 vol_lookback=10 + warmup）
	for i := 0; i < 20; i++ {
		s.OnKline(types.Kline{Close: 100 + float64(i)*2})
	}

	close(signals)
	var buys int
	for sig := range signals {
		if sig.Side == types.SideBuy && !sig.ReduceOnly {
			buys++
		}
	}
	if buys == 0 {
		t.Error("上涨趋势应至少 emit 一次 Buy")
	}
}

// 合约模式：上涨→下跌应触发反向开仓。
func TestVMM_SwapReversal(t *testing.T) {
	s := &vmm{}
	signals := make(chan types.Signal, 128)
	s.Init(Config{
		InstID:   "BTC-USDT-SWAP",
		InstType: types.InstSwap,
		Params: map[string]interface{}{
			"return_lookback":   3,
			"vol_lookback":      10,
			"rebalance_bars":    1,
			"target_vol_annual": 1.0,
			"bars_per_year":     365.0,
			"base_position_usd": 500.0,
			"max_scale":         5.0,
		},
	}, signals)

	// 先上涨后下跌
	for i := 0; i < 15; i++ {
		s.OnKline(types.Kline{Close: 100 + float64(i)*2})
	}
	for i := 0; i < 15; i++ {
		s.OnKline(types.Kline{Close: 130 - float64(i)*2})
	}

	close(signals)
	var openBuy, openSell int
	for sig := range signals {
		if sig.ReduceOnly {
			continue
		}
		switch sig.Side {
		case types.SideBuy:
			openBuy++
		case types.SideSell:
			openSell++
		}
	}
	if openBuy == 0 {
		t.Error("上涨段应至少开过一次多")
	}
	if openSell == 0 {
		t.Error("下跌段应至少开过一次空")
	}
}

// 仓位约束：max_scale=1.0 时低波动期 size_usd 不应超过 base_position_usd。
//
// 这是 VMM 与 TSMOM 的关键区别：VMM 默认无杠杆，仓位 ≤ 100%。
func TestVMM_MaxScaleConstraint(t *testing.T) {
	s := &vmm{}
	signals := make(chan types.Signal, 64)
	s.Init(Config{
		InstID:   "T",
		InstType: types.InstSpot,
		Params: map[string]interface{}{
			"return_lookback":   3,
			"vol_lookback":      10,
			"rebalance_bars":    1,
			"target_vol_annual": 5.0, // 故意设很大，让 scale 会爆
			"bars_per_year":     365.0,
			"base_position_usd": 500.0,
			"max_scale":         1.0, // ★ 严格上限
		},
	}, signals)

	// 极低波动 + 上涨：让 scale 想冲到很大
	for i := 0; i < 20; i++ {
		s.OnKline(types.Kline{Close: 100 + float64(i)*0.01})
	}

	close(signals)
	// 每个买入信号的 size * price 都不应超过 base 500
	for sig := range signals {
		if sig.Side != types.SideBuy || sig.ReduceOnly {
			continue
		}
		notional := sig.Size * 100 // 价格在 100 附近
		if notional > 500*1.05 {
			t.Errorf("仓位超过 max_scale 上限: notional=%.2f > 500", notional)
		}
	}
}

// 波动率缩放对比：高波动期 size 应明显小于低波动期。
func TestVMM_VolScaling(t *testing.T) {
	feed := func(prices []float64) (size float64) {
		s := &vmm{}
		sig := make(chan types.Signal, 32)
		s.Init(Config{
			InstID:   "T",
			InstType: types.InstSpot,
			Params: map[string]interface{}{
				"return_lookback":   3,
				"vol_lookback":      10,
				"rebalance_bars":    1,
				"target_vol_annual": 0.5,
				"bars_per_year":     365.0,
				"base_position_usd": 500.0,
				"max_scale":         10.0, // 放开上限以便观察 scale 差异
			},
		}, sig)
		for _, p := range prices {
			s.OnKline(types.Kline{Close: p})
		}
		close(sig)
		for x := range sig {
			if x.Side == types.SideBuy && !x.ReduceOnly {
				return x.Size
			}
		}
		return 0
	}

	// 低波动：每根 +0.1
	low := make([]float64, 20)
	for i := range low {
		low[i] = 100 + float64(i)*0.1
	}
	// 高波动：之字形上涨
	hi := []float64{100, 95, 105, 100, 110, 105, 115, 110, 120, 115, 125, 120, 130, 125, 135, 130, 140, 135, 145, 140}

	lowSize := feed(low)
	hiSize := feed(hi)
	if lowSize == 0 || hiSize == 0 {
		t.Skipf("未产生买入信号: low=%v, hi=%v", lowSize, hiSize)
	}
	if hiSize >= lowSize {
		t.Errorf("高波动应缩仓: hiSize=%.4f >= lowSize=%.4f", hiSize, lowSize)
	}
	t.Logf("低波动 size=%.4f, 高波动 size=%.4f (比例 %.1fx)",
		lowSize, hiSize, lowSize/hiSize)
}

func newVMMForResize(t *testing.T) (*vmm, chan types.Signal) {
	t.Helper()
	s := &vmm{}
	sigs := make(chan types.Signal, 256)
	cfg := Config{
		InstID:   "ETH-USDT-SWAP",
		InstType: types.InstSwap,
		Params: map[string]interface{}{
			"return_lookback":   7,
			"vol_lookback":      20,
			"rebalance_bars":    1,
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

// feedClosesVMM 复用 tsmom_test 里的 genSeries，但喂给 *vmm。
func feedClosesVMM(s *vmm, closes []float64) {
	for _, c := range closes {
		s.OnKline(types.Kline{InstID: "ETH-USDT-SWAP", Open: c, High: c, Low: c, Close: c})
	}
}

// TestVMM_VolScalingResizesSameDirection 覆盖与 tsmom 相同的核心 bug：
// 方向不变、波动率升高导致目标仓位显著缩小时，VMM 必须真的减仓。
// 旧实现里 rebalance 对同方向直接 return，这个测试会失败。
func TestVMM_VolScalingResizesSameDirection(t *testing.T) {
	s, sigs := newVMMForResize(t)

	// 阶段1：低波动缓涨 → 多头 + 高 scale(撞 cap=3) → 大仓位
	feedClosesVMM(s, genSeries(100, 0.003, 0.002, 40))
	openSize1, ok := lastOpenSize(drainSignalsTest(sigs))
	if !ok {
		t.Fatalf("阶段1 应开多仓，却没有开仓信号")
	}

	// 阶段2：高波动继续上涨 → 仍多头，但 annVol 飙升 → scale 变小 → 目标仓位显著缩小
	feedClosesVMM(s, genSeries(s.closes[len(s.closes)-1], 0.015, 0.04, 40))
	closes, opens := splitSignals(drainSignalsTest(sigs))

	if len(closes) == 0 || len(opens) == 0 {
		t.Fatalf("波动率升高应触发同方向减仓(先平后开)，实际 closes=%d opens=%d。"+
			"vol scaling 没生效。", len(closes), len(opens))
	}
	openSize2 := opens[len(opens)-1].Size
	if openSize2 >= openSize1 {
		t.Fatalf("高波动应缩仓：openSize2=%.4f 应 < openSize1=%.4f", openSize2, openSize1)
	}
	if closes[0].Size <= 0 {
		t.Errorf("平仓 Size 应为正的持仓量, got %.4f", closes[0].Size)
	}
	t.Logf("vol scaling 生效：低波动仓位 %.4f → 高波动仓位 %.4f", openSize1, openSize2)
}

// TestVMM_NoResizeOnSmallDrift 回归保护：同方向且 size 漂移很小时不应频繁调仓。
func TestVMM_NoResizeOnSmallDrift(t *testing.T) {
	s, sigs := newVMMForResize(t)

	feedClosesVMM(s, genSeries(100, 0.003, 0.002, 40)) // 低波动建多仓(撞 cap)
	_ = drainSignalsTest(sigs)

	feedClosesVMM(s, genSeries(s.closes[len(s.closes)-1], 0.003, 0.002, 12))
	after := drainSignalsTest(sigs)
	if len(after) != 0 {
		t.Fatalf("小漂移不应调仓，却收到 %d 个信号: %+v", len(after), after)
	}
}
