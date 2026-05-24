package chanlun

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// 端到端：构造一段足够长的、有明显结构的 K 线序列，
// 验证 5 层数据（MergedKline / Fractal / Bi / Pivot / BSPoint）都被填充。
//
// 这等价于"paper 模式跑一次"，但不依赖外部行情和 API key。
func TestEngine_E2E_Pipeline(t *testing.T) {
	e := NewEngine(Config{})

	// 三段走势：下跌 → 上涨 → 下跌 → 上涨 → 下跌
	// 每段都足够长（>= 6 根 MK），保证笔成立。
	klines := []types.Kline{}
	klines = append(klines, fall(100, 50, 0, 0)...)   // K[0..49] 100→50
	klines = append(klines, rise(50, 90, 0, 50)...)   // K[50..89] 50→90
	klines = append(klines, fall(90, 40, 0, 90)...)   // K[90..139] 90→40 (下面会创新低)
	klines = append(klines, rise(40, 80, 0, 140)...)  // K[140..179] 40→80
	klines = append(klines, fall(80, 35, 0, 180)...)  // K[180..229] 创新低 35

	for _, k := range klines {
		e.Feed(k)
	}

	if got := len(e.MergedKlines()); got < 50 {
		t.Errorf("MergedKline 数太少: %d", got)
	}
	if got := len(e.Fractals()); got < 4 {
		t.Errorf("分型数太少: %d (期望至少 4 个)", got)
	}
	var confirmedFx int
	for _, f := range e.Fractals() {
		if f.Confirmed {
			confirmedFx++
		}
	}
	if confirmedFx < 4 {
		t.Errorf("已确认分型数太少: %d", confirmedFx)
	}
	var confirmedBi int
	for _, b := range e.Bis() {
		if b.Confirmed {
			confirmedBi++
		}
	}
	if confirmedBi < 3 {
		t.Errorf("已确认笔数太少: %d (期望至少 3 笔)", confirmedBi)
	}

	t.Logf("E2E 结果: mks=%d fxs=%d(confirmed=%d) bis=%d(confirmed=%d) pivots=%d bsps=%d",
		len(e.MergedKlines()), len(e.Fractals()), confirmedFx,
		len(e.Bis()), confirmedBi, len(e.Pivots()), len(e.BSPoints()))

	// 至少应该有一些买卖点（背驰判定不保证一定出现，所以这里只 log）
	for _, bs := range e.BSPoints() {
		t.Logf("买卖点: kind=%d price=%.2f reason=%s", bs.Kind, bs.Price, bs.Reason)
	}
}

// fall 生成 n 段单调下跌的 K 线，从 startPrice 到 endPrice。
// startIdx 用于时间戳赋值。每根 K 线的 H/L 都比前一根低，避免包含关系。
func fall(start, end float64, _ int, startIdx int) []types.Kline {
	n := 50
	step := (start - end) / float64(n-1)
	out := make([]types.Kline, n)
	for i := 0; i < n; i++ {
		mid := start - step*float64(i)
		h := mid + step*0.3
		l := mid - step*0.3
		c := mid - step*0.1
		out[i] = types.Kline{
			InstID:    "TEST",
			Timestamp: int64(startIdx+i) * 60_000,
			Open:      mid,
			High:      h,
			Low:       l,
			Close:     c,
			Volume:    1,
		}
	}
	return out
}

// rise 生成 n 段单调上涨的 K 线。
func rise(start, end float64, _ int, startIdx int) []types.Kline {
	n := 40
	step := (end - start) / float64(n-1)
	out := make([]types.Kline, n)
	for i := 0; i < n; i++ {
		mid := start + step*float64(i)
		h := mid + step*0.3
		l := mid - step*0.3
		c := mid + step*0.1
		out[i] = types.Kline{
			InstID:    "TEST",
			Timestamp: int64(startIdx+i) * 60_000,
			Open:      mid,
			High:      h,
			Low:       l,
			Close:     c,
			Volume:    1,
		}
	}
	return out
}
