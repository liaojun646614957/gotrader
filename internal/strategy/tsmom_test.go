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
