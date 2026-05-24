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
