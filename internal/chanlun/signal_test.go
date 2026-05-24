package chanlun

import (
	"testing"
)

// 直接测纯函数 check1stBS，避免构造完整 MACD 场景的复杂度。
// 端到端验证留给 chan_bs 策略的集成测试 / paper 运行。

func TestCheck1stBS_BuyBottomDivergence(t *testing.T) {
	// 参考笔：下跌，从 100 到 80，MACD 面积 20
	bRef := Bi{
		Dir:        DirDown,
		From:       Fractal{Dir: DirUp, Price: 100, MKIdx: 0},
		To:         Fractal{Dir: DirDown, Price: 80, MKIdx: 10},
		StartMKIdx: 0,
		EndMKIdx:   10,
		MACDArea:   20.0,
		Confirmed:  true,
	}
	// 当前笔：下跌，创新低到 75，但 MACD 面积只有 12（动能减弱）→ 底背驰
	bN := Bi{
		Dir:        DirDown,
		From:       Fractal{Dir: DirUp, Price: 90, MKIdx: 20},
		To:         Fractal{Dir: DirDown, Price: 75, MKIdx: 30},
		StartMKIdx: 20,
		EndMKIdx:   30,
		MACDArea:   12.0,
		Confirmed:  true,
	}
	bs, ok := check1stBS(bN, bRef)
	if !ok {
		t.Fatal("应触发 1 类买点（底背驰）")
	}
	if bs.Kind != 1 {
		t.Errorf("Kind 应为 1, got %d", bs.Kind)
	}
	if bs.Price != 75 {
		t.Errorf("触发价应为新低 75, got %v", bs.Price)
	}
}

func TestCheck1stBS_SellTopDivergence(t *testing.T) {
	// 参考笔：上涨 80→120，MACD 面积 25
	bRef := Bi{
		Dir:        DirUp,
		From:       Fractal{Dir: DirDown, Price: 80},
		To:         Fractal{Dir: DirUp, Price: 120, MKIdx: 10},
		EndMKIdx:   10,
		MACDArea:   25.0,
		Confirmed:  true,
	}
	// 当前笔：上涨创新高 125，但 MACD 面积 15 → 顶背驰
	bN := Bi{
		Dir:        DirUp,
		From:       Fractal{Dir: DirDown, Price: 100},
		To:         Fractal{Dir: DirUp, Price: 125, MKIdx: 30},
		EndMKIdx:   30,
		MACDArea:   15.0,
		Confirmed:  true,
	}
	bs, ok := check1stBS(bN, bRef)
	if !ok {
		t.Fatal("应触发 1 类卖点（顶背驰）")
	}
	if bs.Kind != -1 {
		t.Errorf("Kind 应为 -1, got %d", bs.Kind)
	}
	if bs.Price != 125 {
		t.Errorf("触发价应为新高 125, got %v", bs.Price)
	}
}

// 创新低但 MACD 面积反而变大 → 趋势加速，非背驰，不触发。
func TestCheck1stBS_NotDivergence(t *testing.T) {
	bRef := Bi{
		Dir:      DirDown,
		To:       Fractal{Price: 80, MKIdx: 10},
		MACDArea: 10.0,
	}
	bN := Bi{
		Dir:      DirDown,
		To:       Fractal{Price: 75, MKIdx: 30},
		EndMKIdx: 30,
		MACDArea: 18.0, // 反而变大 = 加速下跌
	}
	if _, ok := check1stBS(bN, bRef); ok {
		t.Error("MACD 面积变大不应触发背驰")
	}
}

// 价格未创新低 → 不触发。
func TestCheck1stBS_NoNewLow(t *testing.T) {
	bRef := Bi{
		Dir:      DirDown,
		To:       Fractal{Price: 80, MKIdx: 10},
		MACDArea: 10.0,
	}
	bN := Bi{
		Dir:      DirDown,
		To:       Fractal{Price: 82, MKIdx: 30},
		EndMKIdx: 30,
		MACDArea: 5.0,
	}
	if _, ok := check1stBS(bN, bRef); ok {
		t.Error("未创新低不应触发 1 类买点")
	}
}

// MACD 面积工具：同向计数正确。
func TestMACDArea_Direction(t *testing.T) {
	m := newMACD(12, 26, 9)
	// 喂一些数据让 hist 有正有负
	prices := []float64{
		100, 101, 99, 98, 100, 102, 105, 107, 110, 108,
		106, 103, 100, 98, 97, 95, 93, 90, 88, 87,
		85, 83, 82, 80, 79, 80, 82, 85, 88, 90,
	}
	for _, p := range prices {
		m.Feed(p)
	}
	if m.Len() != len(prices) {
		t.Fatalf("MACD Len 错: got %d want %d", m.Len(), len(prices))
	}
	// 下行面积应 > 0（前 20 根有下跌）
	areaDown := m.AreaInRange(0, len(prices)-1, DirDown)
	areaUp := m.AreaInRange(0, len(prices)-1, DirUp)
	if areaDown <= 0 && areaUp <= 0 {
		t.Errorf("areaDown=%v areaUp=%v 都为 0，数据有问题", areaDown, areaUp)
	}
}
