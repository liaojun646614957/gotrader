package chanlun

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// helper：构造一根 K 线（时间戳按下标递增）。
func mk(idx int, high, low, close float64) types.Kline {
	return types.Kline{
		InstID:    "TEST",
		Timestamp: int64(idx) * 60_000,
		Open:      (high + low) / 2,
		High:      high,
		Low:       low,
		Close:     close,
		Volume:    1,
	}
}

func feedAll(e *Engine, ks []types.Kline) {
	for _, k := range ks {
		e.Feed(k)
	}
}

// 包含关系处理：上行序列中，第二根被第一根包含，应合并为一根。
func TestMergeKline_ContainsUp(t *testing.T) {
	e := NewEngine(Config{})
	// 三根 K 线：第二根被第一根包含（H=12, L=9 在 H=15, L=8 之内）
	// 假设方向向上，合并应取高高低高 → 第一根的扩张版
	feedAll(e, []types.Kline{
		mk(0, 10, 5, 8),    // 起始
		mk(1, 15, 8, 13),   // 突破，向上
		mk(2, 14, 10, 12),  // 被第二根包含（14<15 && 10>8）→ 与第二根合并
		mk(3, 18, 12, 16),  // 继续向上
	})

	mks := e.MergedKlines()
	if len(mks) != 3 {
		t.Fatalf("期望 3 根 MK，实际 %d；mks=%+v", len(mks), mks)
	}
	// 合并后的那一根：取高高低高 → High=max(15,14)=15, Low=max(8,10)=10
	if mks[1].High != 15 || mks[1].Low != 10 {
		t.Errorf("合并 K 错：期望 H=15 L=10，实际 H=%v L=%v", mks[1].High, mks[1].Low)
	}
}

// 包含关系处理：下行序列中，被包含的合并取低低高低。
func TestMergeKline_ContainsDown(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 20, 15, 17),  // 起始
		mk(1, 18, 10, 12),  // 向下
		mk(2, 16, 11, 13),  // 被第二根包含 → 低低高低
		mk(3, 14, 8, 9),    // 继续向下
	})
	mks := e.MergedKlines()
	if len(mks) != 3 {
		t.Fatalf("期望 3 根 MK，实际 %d", len(mks))
	}
	// 合并：High=min(18,16)=16, Low=min(10,11)=10
	if mks[1].High != 16 || mks[1].Low != 10 {
		t.Errorf("下行合并错：期望 H=16 L=10，实际 H=%v L=%v", mks[1].High, mks[1].Low)
	}
}

// 不存在包含关系时不应合并。
func TestMergeKline_NoContain(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 10, 5, 8),
		mk(1, 12, 7, 11), // 高/低都 > 前一根 → 向上独立
		mk(2, 15, 9, 14), // 继续独立
	})
	mks := e.MergedKlines()
	if len(mks) != 3 {
		t.Errorf("期望 3 根 MK，实际 %d", len(mks))
	}
}

// 原始下标映射：合并后的 MK 的 EndRawIdx 应是被合并的最末根。
func TestMergeKline_RawIdxMapping(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 10, 5, 8),
		mk(1, 15, 8, 13), // 独立
		mk(2, 14, 10, 12), // 与上一根合并
		mk(3, 18, 12, 16),
	})
	mks := e.MergedKlines()
	// mks[1] 由 raw[1] 和 raw[2] 合并，期望 StartRawIdx=1, EndRawIdx=2
	if mks[1].StartRawIdx != 1 || mks[1].EndRawIdx != 2 {
		t.Errorf("RawIdx 映射错: StartRawIdx=%d EndRawIdx=%d, want 1,2",
			mks[1].StartRawIdx, mks[1].EndRawIdx)
	}
}
