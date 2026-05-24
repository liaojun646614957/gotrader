package chanlun

import "github.com/kraus/gotrader/internal/types"

// mergeKline 增量处理一根原始 K 线，更新 e.mks。
//
// 缠论包含关系处理规则：
//
//	若当前 K 与最末 MergedKline 有包含关系（一个完全包住另一个），则合并：
//	  - 向上方向：取 max(High), max(Low)  —— "高高低高"
//	  - 向下方向：取 min(High), min(Low)  —— "低低高低"
//	  - 方向由"最末 MergedKline 相对其前一根"的方向决定。
//	若无包含关系：作为新 MergedKline 追加。
//
// 边界：
//   - 第 1 根：直接追加，Dir=DirNone。
//   - 第 2 根（与第 1 根有包含关系时）：方向尚未确定。
//     按通行做法，初始用"假设向上"处理（OKX 行情绝大多数 K 线相邻不包含，
//     即便这一根错了，第 3 根来时通常就被覆盖）。这里取保守做法：
//     发生时按第 2 根的高低取大者作为合并基准。
//     这是一个无奈的妥协，缠论原文未明确处理此初始边界。
func (e *Engine) mergeKline(k types.Kline) {
	// 注意：rawCount 已在 Feed 入口 ++，当前根的原始下标 = rawCount - 1
	rawIdx := e.rawCount - 1

	cur := MergedKline{
		High:        k.High,
		Low:         k.Low,
		StartTS:     k.Timestamp,
		EndTS:       k.Timestamp,
		StartRawIdx: rawIdx,
		EndRawIdx:   rawIdx,
		Dir:         DirNone,
	}

	n := len(e.mks)
	if n == 0 {
		e.mks = append(e.mks, cur)
		e.macd.Feed(k.Close)
		return
	}

	last := &e.mks[n-1]
	if !contains(*last, cur) {
		// 无包含：直接追加，并赋方向。
		cur.Dir = relDir(*last, cur)
		e.mks = append(e.mks, cur)
		e.macd.Feed(k.Close)
		return
	}

	// 有包含：合并到 last。方向取 last.Dir；last 是第一根时取 DirUp 作初始假设。
	dir := last.Dir
	if dir == DirNone {
		dir = DirUp
	}
	merged := mergeBy(*last, cur, dir)
	merged.Dir = last.Dir
	merged.StartTS = last.StartTS
	merged.EndTS = cur.EndTS
	merged.StartRawIdx = last.StartRawIdx
	merged.EndRawIdx = rawIdx
	e.mks[n-1] = merged

	// MACD 仍按原始 K 线推进。
	e.macd.Feed(k.Close)
}

// contains 判断 a 是否包含 b 或 b 是否包含 a。
func contains(a, b MergedKline) bool {
	return (a.High >= b.High && a.Low <= b.Low) ||
		(b.High >= a.High && b.Low <= a.Low)
}

// relDir b 相对 a 的方向。
func relDir(a, b MergedKline) Direction {
	if b.High > a.High && b.Low > a.Low {
		return DirUp
	}
	if b.High < a.High && b.Low < a.Low {
		return DirDown
	}
	// 不应到达：调用前已检查无包含关系，相邻两根 MK 不会出现"一高一低相等"。
	return DirNone
}

// mergeBy 按指定方向合并 a 和 b。
func mergeBy(a, b MergedKline, dir Direction) MergedKline {
	out := a
	switch dir {
	case DirUp:
		// 高高低高
		out.High = max64(a.High, b.High)
		out.Low = max64(a.Low, b.Low)
	case DirDown:
		out.High = min64(a.High, b.High)
		out.Low = min64(a.Low, b.Low)
	default:
		// 不应到达
		out.High = max64(a.High, b.High)
		out.Low = max64(a.Low, b.Low)
	}
	return out
}

func max64(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
