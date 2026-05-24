package chanlun

import (
	"testing"

	"github.com/kraus/gotrader/internal/types"
)

// 笔形成：从底分型到顶分型，中间间隔满足 5 根 MK。
func TestBi_FormUp(t *testing.T) {
	e := NewEngine(Config{})
	// 下跌 → 底分型（MK4）→ 上涨 → 顶分型（MK9）→ 右侧确认
	feedAll(e, []types.Kline{
		mk(0, 20, 18, 19),
		mk(1, 18, 16, 17),
		mk(2, 16, 14, 15),
		mk(3, 14, 12, 13),
		mk(4, 12, 10, 11), // 底
		mk(5, 14, 11, 13),
		mk(6, 16, 13, 15),
		mk(7, 18, 15, 17),
		mk(8, 20, 17, 19),
		mk(9, 22, 19, 21), // 顶
		mk(10, 18, 15, 16),
		mk(11, 15, 12, 13),
	})
	bis := e.Bis()
	var confirmed []Bi
	for _, b := range bis {
		if b.Confirmed {
			confirmed = append(confirmed, b)
		}
	}
	if len(confirmed) == 0 {
		t.Fatalf("期望至少 1 条已确认笔，实际 0; bis=%+v", bis)
	}
	first := confirmed[0]
	if first.Dir != DirUp {
		t.Errorf("第一笔方向应为 DirUp, got %v", first.Dir)
	}
	// 分型取的是 MK 的 High/Low 极值，不是 close。底=10（K4.Low），顶=22（K9.High）。
	if first.From.Price != 10 || first.To.Price != 22 {
		t.Errorf("第一笔起止价错：from=%v to=%v, want 10 -> 22", first.From.Price, first.To.Price)
	}
}

// 间距不够时不应成笔。
func TestBi_NotEnoughMKs(t *testing.T) {
	e := NewEngine(Config{})
	// 底分型在 MK1，顶分型在 MK3，间距 2 < MinBiMKs(5)-1=4 → 不成笔
	feedAll(e, []types.Kline{
		mk(0, 15, 13, 14),
		mk(1, 13, 10, 11), // 底分型候选
		mk(2, 15, 12, 14),
		mk(3, 17, 14, 16), // 顶分型候选
		mk(4, 15, 12, 13),
	})
	bis := e.Bis()
	for _, b := range bis {
		if b.Confirmed {
			t.Errorf("间距太短不应成已确认笔, got %+v", b)
		}
	}
}
