package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

// testStrategy 测试用：在指定 K 线下标触发买/卖/平仓。
//
// 不通过 strategy 包的注册表，直接构造实例传给 Runner。
// buyAt/sellAt < 0 表示"不触发"。
// closeAt 为 ReduceOnly 平仓（按当前持仓方向反向 emit）。
type testStrategy struct {
	signals chan<- types.Signal
	cfg     strategy.Config
	count   int
	buyAt   int
	sellAt  int
	closeAt int // 在该下标 emit 一个 reduce-only 信号（默认 -1 不触发）
	size    float64
	openSide types.Side // 记录开仓方向供平仓选反向
}

func (s *testStrategy) Init(cfg strategy.Config, ch chan<- types.Signal) error {
	s.cfg = cfg
	s.signals = ch
	return nil
}
func (s *testStrategy) WarmupBars() int             { return 0 }
func (s *testStrategy) OnTick(t types.Tick)         {}
func (s *testStrategy) OnOrderUpdate(o types.Order) {}

func (s *testStrategy) OnKline(k types.Kline) {
	idx := s.count
	s.count++
	if idx == s.buyAt {
		s.signals <- types.Signal{
			InstID:   s.cfg.InstID,
			InstType: s.cfg.InstType,
			Side:     types.SideBuy,
			Type:     types.OrderMarket,
			Size:     s.size,
			Reason:   "test buy",
		}
		s.openSide = types.SideBuy
	}
	if idx == s.sellAt {
		s.signals <- types.Signal{
			InstID:   s.cfg.InstID,
			InstType: s.cfg.InstType,
			Side:     types.SideSell,
			Type:     types.OrderMarket,
			Size:     s.size,
			Reason:   "test sell",
		}
		s.openSide = types.SideSell
	}
	if s.closeAt > 0 && idx == s.closeAt {
		// 反向 reduce-only 平仓
		closeSide := types.SideSell
		if s.openSide == types.SideSell {
			closeSide = types.SideBuy
		}
		s.signals <- types.Signal{
			InstID:     s.cfg.InstID,
			InstType:   s.cfg.InstType,
			Side:       closeSide,
			Type:       types.OrderMarket,
			Size:       s.size,
			ReduceOnly: true,
			Reason:     "test close",
		}
	}
}

func mkK(idx int, o, h, l, c float64) types.Kline {
	return types.Kline{
		InstID:    "TEST",
		Timestamp: int64(idx) * 60_000,
		Open:      o,
		High:      h,
		Low:       l,
		Close:     c,
		Volume:    1,
	}
}

// 必赢场景：低买高卖，验证 PnL/胜率/手续费正确。
func TestRunner_WinningTrade(t *testing.T) {
	strat := &testStrategy{buyAt: 1, sellAt: 3, size: 1.0}
	r, err := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSpot},
		strat,
	)
	if err != nil {
		t.Fatal(err)
	}

	// K0..K4，索引 1 时买，索引 3 时卖
	// 撮合是"下一根 open"：买在 K2.Open=10，卖在 K4.Open=12
	klines := []types.Kline{
		mkK(0, 9, 9, 9, 9),
		mkK(1, 9.5, 9.5, 9.5, 9.5),
		mkK(2, 10, 10, 10, 10),
		mkK(3, 11, 11, 11, 11),
		mkK(4, 12, 12, 12, 12),
	}

	stats, err := r.Run(klines)
	if err != nil {
		t.Fatal(err)
	}

	trades := r.Trades()
	if len(trades) != 2 {
		t.Fatalf("应有 2 笔成交，实际 %d", len(trades))
	}

	// 买：price=10, size=1, fee=0.01
	if !almostEqual(trades[0].Price, 10) || !almostEqual(trades[0].Size, 1.0) {
		t.Errorf("买入错: %+v", trades[0])
	}
	if !almostEqual(trades[0].Fee, 10*0.001) {
		t.Errorf("买入手续费错: got %v, want %v", trades[0].Fee, 0.01)
	}

	// 卖：price=12, size=1, fee=0.012, PnL = (12-10)*1 - 0.012 = 1.988
	if !almostEqual(trades[1].Price, 12) {
		t.Errorf("卖出价错: %+v", trades[1])
	}
	expectPnL := (12-10)*1.0 - 12*0.001
	if !almostEqual(trades[1].PnL, expectPnL) {
		t.Errorf("PnL 错: got %v, want %v", trades[1].PnL, expectPnL)
	}

	if stats.Wins != 1 || stats.Losses != 0 {
		t.Errorf("胜负数错: wins=%d losses=%d", stats.Wins, stats.Losses)
	}
	if !almostEqual(stats.WinRate, 1.0) {
		t.Errorf("胜率应为 100%%, got %v", stats.WinRate)
	}
}

// 必输场景：高买低卖，验证负 PnL 与亏损统计。
func TestRunner_LosingTrade(t *testing.T) {
	strat := &testStrategy{buyAt: 1, sellAt: 3, size: 1.0}
	r, _ := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSpot},
		strat,
	)
	klines := []types.Kline{
		mkK(0, 100, 100, 100, 100),
		mkK(1, 100, 100, 100, 100),
		mkK(2, 100, 100, 100, 100), // 买入价
		mkK(3, 95, 95, 95, 95),
		mkK(4, 90, 90, 90, 90), // 卖出价
	}
	stats, err := r.Run(klines)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Losses != 1 || stats.Wins != 0 {
		t.Errorf("应为 1 亏 0 胜, got wins=%d losses=%d", stats.Wins, stats.Losses)
	}
	if stats.NetPnL >= 0 {
		t.Errorf("NetPnL 应为负, got %v", stats.NetPnL)
	}
}

// 合约双向：开空、平空、PnL 计算。
//
// 场景：高位 100 开空 1 手，低位 80 平空 → PnL = (100-80)*1 - fees
func TestRunner_ShortTrade(t *testing.T) {
	strat := &testStrategy{buyAt: -1, sellAt: 1, closeAt: 3, size: 1.0}
	r, _ := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSwap},
		strat,
	)
	klines := []types.Kline{
		mkK(0, 100, 100, 100, 100),
		mkK(1, 100, 100, 100, 100),
		mkK(2, 100, 100, 100, 100), // 开空价
		mkK(3, 90, 90, 90, 90),
		mkK(4, 80, 80, 80, 80), // 平空价
	}
	stats, err := r.Run(klines)
	if err != nil {
		t.Fatal(err)
	}
	trades := r.Trades()
	if len(trades) != 2 {
		t.Fatalf("应有 2 笔成交，实际 %d", len(trades))
	}
	// 开空：fee = 100*1*0.001 = 0.1
	if !almostEqual(trades[0].Fee, 0.1) {
		t.Errorf("开空手续费错: %v 应=0.1", trades[0].Fee)
	}
	// 平空 PnL：(100-80)*1 = 20，扣平仓 fee = 80*1*0.001 = 0.08 → 19.92
	expectPnL := 20.0 - 0.08
	if !almostEqual(trades[1].PnL, expectPnL) {
		t.Errorf("空头 PnL 错: %v 应=%v", trades[1].PnL, expectPnL)
	}
	if stats.Wins != 1 || stats.Losses != 0 {
		t.Errorf("空头盈利场景胜负数错: wins=%d losses=%d", stats.Wins, stats.Losses)
	}
}

// SPOT 模式下不允许做空：Sell 信号空仓时被视为"目标=0"，等于无效操作。
func TestRunner_SpotRejectsShort(t *testing.T) {
	strat := &testStrategy{buyAt: -1, sellAt: 1, size: 1.0}
	r, _ := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSpot},
		strat,
	)
	klines := []types.Kline{
		mkK(0, 100, 100, 100, 100),
		mkK(1, 100, 100, 100, 100),
		mkK(2, 100, 100, 100, 100),
	}
	if _, err := r.Run(klines); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Trades()); got != 0 {
		t.Errorf("SPOT 模式空仓时收到 Sell 不应成交, got %d 笔", got)
	}
}

// 反转测试：先开多，再收到 Sell 信号（合约模式）→ 平多 + 开空 一气呵成。
func TestRunner_ReversalLongToShort(t *testing.T) {
	strat := &testStrategy{buyAt: 0, sellAt: 2, size: 1.0}
	r, _ := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSwap},
		strat,
	)
	klines := []types.Kline{
		mkK(0, 100, 100, 100, 100), // K0 开多信号
		mkK(1, 100, 100, 100, 100), // 开多撮合价
		mkK(2, 110, 110, 110, 110), // K2 反转信号（持多浮盈+10）
		mkK(3, 120, 120, 120, 120), // 反转撮合价（在 120 平多+开空）
		mkK(4, 100, 100, 100, 100), // 价格回落，浮盈空头
	}
	if _, err := r.Run(klines); err != nil {
		t.Fatal(err)
	}
	// 此时应：持有 -1（空头），avgPrice=120
	if r.qty >= 0 {
		t.Errorf("反转后应为空头, qty=%v", r.qty)
	}
	if !almostEqual(r.avgPrice, 120) {
		t.Errorf("反转后 avgPrice 应=120(K3.Open), got %v", r.avgPrice)
	}
	// K4 收盘价 100，空头浮盈 (120-100)*1 = 20
	finalEquity := r.equity(100)
	// 已实现：开多 100 → 平多 120 = +20；手续费总共: 开多 0.1 + 平多 0.12 + 开空 0.12 = 0.34
	// 浮盈：(120-100)*(-(-1)) = 20
	// 总权益 = 1000 + 20 - 0.34 + 20 = 1039.66
	expected := 1000 + 20 - 0.34 + 20
	if !almostEqual(finalEquity, expected) {
		t.Errorf("反转后权益错: got %v, want %v", finalEquity, expected)
	}
}

// 没信号的策略：应零成交，权益不变（手续费也为零）。
func TestRunner_NoSignals(t *testing.T) {
	strat := &testStrategy{buyAt: -1, sellAt: -1}
	r, _ := NewRunner(
		Config{FeeRate: 0.001, InitEquity: 1000},
		strategy.Config{InstID: "TEST", InstType: types.InstSpot},
		strat,
	)
	klines := []types.Kline{
		mkK(0, 100, 100, 100, 100),
		mkK(1, 110, 110, 110, 110),
		mkK(2, 90, 90, 90, 90),
	}
	stats, _ := r.Run(klines)
	if stats.TotalTrades != 0 {
		t.Errorf("应为 0 成交, got %d", stats.TotalTrades)
	}
	if !almostEqual(stats.FinalEquity, 1000) {
		t.Errorf("无成交时权益应不变, got %v", stats.FinalEquity)
	}
}

// inferBarsPerYear 应根据时间戳推算正确周期。
func TestInferBarsPerYear(t *testing.T) {
	cases := []struct {
		name      string
		barSec    int64
		wantApprox float64
	}{
		{"5m K线", 5 * 60, 105120},
		{"1H K线", 60 * 60, 8760},
		{"1D K线", 24 * 60 * 60, 365},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			curve := make([]EquityPoint, 11)
			t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			for i := range curve {
				curve[i] = EquityPoint{Time: t0.Add(time.Duration(i*int(c.barSec)) * time.Second)}
			}
			got := inferBarsPerYear(curve)
			diff := math.Abs(got-c.wantApprox) / c.wantApprox
			if diff > 0.001 {
				t.Errorf("got %.0f, want %.0f", got, c.wantApprox)
			}
		})
	}
}

// Sharpe 计算：构造已知收益序列，对答案。
func TestSharpeCalc(t *testing.T) {
	// 10 根日线，每根权益涨 1%：r = ln(1.01) ≈ 0.00995 每天
	// 年化收益 ≈ 0.00995 * 365 ≈ 3.63
	// 年化波动 ≈ 0 * sqrt(365) ≈ 0 → Sharpe 无穷大
	// 加点扰动让 vol > 0
	curve := []EquityPoint{}
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	values := []float64{100, 101, 100, 102, 101, 103, 102, 104, 103, 105}
	for i, v := range values {
		curve = append(curve, EquityPoint{
			Time:   t0.Add(time.Duration(i*24) * time.Hour),
			Equity: v,
		})
	}
	annRet, annVol, sharpe, sortino := riskAdjusted(curve, 365)
	if annRet <= 0 {
		t.Errorf("上升趋势 annRet 应 > 0, got %v", annRet)
	}
	if annVol <= 0 {
		t.Errorf("有波动 annVol 应 > 0, got %v", annVol)
	}
	if sharpe <= 0 {
		t.Errorf("正期望策略 Sharpe 应 > 0, got %v", sharpe)
	}
	if sortino <= sharpe {
		// Sortino 通常 > Sharpe（因为只惩罚下行）
		t.Logf("注意: Sortino=%v <= Sharpe=%v (小样本可能反常)", sortino, sharpe)
	}
	t.Logf("annRet=%.4f annVol=%.4f Sharpe=%.3f Sortino=%.3f",
		annRet, annVol, sharpe, sortino)
}

// 最大回撤计算。
func TestMaxDrawdown(t *testing.T) {
	vals := []float64{100, 120, 110, 130, 90, 105, 140}
	curve := make([]EquityPoint, len(vals))
	for i, v := range vals {
		curve[i] = EquityPoint{Equity: v}
	}
	// 走势：100 -> 120 -> 110 (DD=10) -> 130 -> 90 (DD=40，从130) -> 105 -> 140
	// 最大回撤 = 40，pct = 40/130
	abs, pct := maxDrawdown(curve)
	if !almostEqual(abs, 40) {
		t.Errorf("最大回撤值错: got %v want 40", abs)
	}
	if !almostEqual(pct, 40.0/130.0) {
		t.Errorf("最大回撤比例错: got %v want %v", pct, 40.0/130.0)
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}
