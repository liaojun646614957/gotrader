package chanlun

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// 顶分型识别：3 根 K 线，中间最高 → 顶分型。
// 第 4 根用来确认（FractalRightConfirm=1）。
func TestFractal_Top(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 10, 5, 7),
		mk(1, 12, 7, 11), // 向上
		mk(2, 15, 10, 14), // 顶
		mk(3, 13, 8, 9),   // 回落，左侧已成顶分型
		mk(4, 11, 6, 7),   // 右侧确认
	})
	fxs := e.Fractals()
	if len(fxs) == 0 {
		t.Fatal("期望识别到顶分型，实际无")
	}
	// 找顶分型
	var top *Fractal
	for i := range fxs {
		if fxs[i].Dir == DirUp {
			top = &fxs[i]
			break
		}
	}
	if top == nil {
		t.Fatalf("应识别到顶分型, fxs=%+v", fxs)
	}
	if top.Price != 15 {
		t.Errorf("顶分型价格错：want 15, got %v", top.Price)
	}
	if !top.Confirmed {
		t.Errorf("顶分型应已确认（右侧 2 根独立 K 已走出）")
	}
}

// 底分型识别。
func TestFractal_Bottom(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 20, 15, 17),
		mk(1, 18, 10, 12), // 向下
		mk(2, 15, 5, 8),   // 底
		mk(3, 17, 9, 14),  // 回升
		mk(4, 19, 12, 17), // 右侧确认
	})
	fxs := e.Fractals()
	var bot *Fractal
	for i := range fxs {
		if fxs[i].Dir == DirDown {
			bot = &fxs[i]
			break
		}
	}
	if bot == nil {
		t.Fatalf("应识别到底分型, fxs=%+v", fxs)
	}
	if bot.Price != 5 {
		t.Errorf("底分型价格错：want 5, got %v", bot.Price)
	}
	if !bot.Confirmed {
		t.Errorf("底分型应已确认")
	}
}

// 分型被破坏：刚出现的顶后面又有更高的高点 → 不应确认。
func TestFractal_NotConfirmedWhenBroken(t *testing.T) {
	e := NewEngine(Config{})
	feedAll(e, []types.Kline{
		mk(0, 10, 5, 7),
		mk(1, 12, 7, 11),
		mk(2, 15, 10, 14), // 候选顶
		mk(3, 18, 13, 17), // 但价格继续创新高 → 顶被破坏
	})
	fxs := e.Fractals()
	for _, f := range fxs {
		if f.Dir == DirUp && f.Price == 15 && f.Confirmed {
			t.Errorf("顶分型被新高破坏后仍标记为已确认: %+v", f)
		}
	}
}
