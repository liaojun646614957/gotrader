package chanlun

// buildBis 基于已识别的分型构建笔序列。
//
// 算法（贪心单遍 + 最后一笔可未确认）：
//
//  1. 从 e.fxs 中取所有 Confirmed=true 的分型，按时间顺序处理：
//     - 维护"已采用分型链" accepted（相邻必异向）。
//     - 新分型 f 与 accepted 尾 t 比较：
//     · 同向且 f 更优（顶更高 / 底更低）→ 替换 t。
//     · 异向且 f.MKIdx - t.MKIdx >= MinBiMKs-1 → 追加。
//     · 异向但距离不够 → 丢弃 f（不影响 t 的有效性）。
//
//  2. 相邻 accepted 两两组成已确认笔。计算 MACD 面积。
//
//  3. 最后再看一根"潜在末笔"（用最后一个已确认或未确认分型，
//     只要与 accepted 尾异向且间距够），生成 Confirmed=false 的临时笔。
//
// 每次重建，时间复杂度 O(N_fx)。N_fx 通常远小于 N_mk，可接受。
func (e *Engine) buildBis() {
	e.bis = e.bis[:0]

	accepted := make([]Fractal, 0, len(e.fxs))
	for _, f := range e.fxs {
		if !f.Confirmed {
			continue
		}
		accepted = append2(accepted, f, e.cfg.MinBiMKs)
	}

	// 配对生成已确认笔
	for i := 1; i < len(accepted); i++ {
		from, to := accepted[i-1], accepted[i]
		bi := Bi{
			Dir:        biDir(from, to),
			From:       from,
			To:         to,
			StartMKIdx: from.MKIdx,
			EndMKIdx:   to.MKIdx,
			Confirmed:  true,
		}
		bi.MACDArea = e.computeBiMACDArea(bi)
		e.bis = append(e.bis, bi)
	}

	// 末笔（潜在未确认）
	if last := e.tentativeLastBi(accepted); last != nil {
		e.bis = append(e.bis, *last)
	}
}

// append2 将分型 f 按贪心规则加入 acc。
// minMKs：笔的最小 MergedKline 数（含两端）。
func append2(acc []Fractal, f Fractal, minMKs int) []Fractal {
	n := len(acc)
	if n == 0 {
		return append(acc, f)
	}
	t := acc[n-1]
	if t.Dir == f.Dir {
		// 同向：取更优
		better := false
		if t.Dir == DirUp && f.Price > t.Price {
			better = true
		}
		if t.Dir == DirDown && f.Price < t.Price {
			better = true
		}
		if better {
			acc[n-1] = f
		}
		return acc
	}
	// 异向：检查距离
	if f.MKIdx-t.MKIdx >= minMKs-1 {
		return append(acc, f)
	}
	// 距离不够：丢弃 f
	return acc
}

// tentativeLastBi 用最后一个分型（含未确认）尝试构造潜在末笔。
// 返回的 Bi 必然 Confirmed=false。
func (e *Engine) tentativeLastBi(accepted []Fractal) *Bi {
	if len(accepted) == 0 || len(e.fxs) == 0 {
		return nil
	}
	tail := accepted[len(accepted)-1]

	// 找最后一个与 tail 异向、且距离满足的分型（不要求 Confirmed）
	for i := len(e.fxs) - 1; i >= 0; i-- {
		f := e.fxs[i]
		if f.Dir == tail.Dir {
			continue
		}
		if f.MKIdx <= tail.MKIdx {
			break
		}
		if f.MKIdx-tail.MKIdx < e.cfg.MinBiMKs-1 {
			continue
		}
		bi := Bi{
			Dir:        biDir(tail, f),
			From:       tail,
			To:         f,
			StartMKIdx: tail.MKIdx,
			EndMKIdx:   f.MKIdx,
			Confirmed:  false,
		}
		bi.MACDArea = e.computeBiMACDArea(bi)
		return &bi
	}
	return nil
}

// biDir 由 from→to 推出笔方向。
func biDir(from, to Fractal) Direction {
	if to.Price > from.Price {
		return DirUp
	}
	return DirDown
}

// computeBiMACDArea 计算一笔覆盖的"同向" MACD 柱面积。
//
// 范围 = [mks[StartMKIdx].StartRawIdx, mks[EndMKIdx].EndRawIdx]
// 方向 = 笔方向
//
// 上涨笔取 hist>0 的累加，下跌笔取 hist<0 的累加绝对值。这样
// 弱反弹/弱回调被过滤，背驰比较更稳。
func (e *Engine) computeBiMACDArea(b Bi) float64 {
	if b.StartMKIdx < 0 || b.EndMKIdx >= len(e.mks) {
		return 0
	}
	from := e.mks[b.StartMKIdx].StartRawIdx
	to := e.mks[b.EndMKIdx].EndRawIdx
	return e.macd.AreaInRange(from, to, b.Dir)
}
