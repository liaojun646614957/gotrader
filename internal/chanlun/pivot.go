package chanlun

// updatePivots 增量重建笔中枢序列。
//
// 笔中枢（笔中枢，MVP 简化版）：
//
//  1. 取连续三笔 b[i], b[i+1], b[i+2]，求它们价格区间的交集：
//     High = min(三笔高点)，Low = max(三笔底点)。
//     若 High > Low，形成中枢。
//
//  2. 中枢"延续"：从 b[i+3] 开始向后扫描，只要笔与中枢有价格交集（不完全脱离），
//     就视为中枢内的回拉/穿越，BiIdxs 追加。
//
//  3. 中枢"破坏"：出现一笔与中枢区间无任何交集时停止，标记 Broken=true。
//     从那一笔后开始尝试构造下一中枢。
//
// 没区分"中枢扩展"和"中枢延续"——MVP 不需要那么细。
//
// 每次 Feed 都重算。N_bi 通常很小，时间复杂度可接受。
func (e *Engine) updatePivots() {
	e.pivots = e.pivots[:0]

	// 只用已确认笔
	bis := make([]Bi, 0, len(e.bis))
	biIdxMap := make([]int, 0, len(e.bis)) // 对应原始 e.bis 下标
	for i, b := range e.bis {
		if b.Confirmed {
			bis = append(bis, b)
			biIdxMap = append(biIdxMap, i)
		}
	}

	n := len(bis)
	i := 0
	for i <= n-3 {
		hi, lo, ok := intersect3(bis[i], bis[i+1], bis[i+2])
		if !ok {
			i++
			continue
		}
		// 中枢形成，扫描延续
		idxs := []int{biIdxMap[i], biIdxMap[i+1], biIdxMap[i+2]}
		j := i + 3
		broken := false
		for j < n {
			if biIntersects(bis[j], hi, lo) {
				idxs = append(idxs, biIdxMap[j])
				j++
			} else {
				broken = true
				break
			}
		}
		dir := bis[i].Dir
		e.pivots = append(e.pivots, Pivot{
			High:   hi,
			Low:    lo,
			BiIdxs: idxs,
			Dir:    dir,
			Broken: broken,
		})
		i = j
		if !broken {
			// 中枢未破坏（已经到序列末尾），跳出循环避免与下一中枢重叠
			break
		}
		// 破坏的话，从破坏那一笔之后继续尝试下一中枢
	}
}

// intersect3 三笔的价格区间交集。
func intersect3(a, b, c Bi) (high, low float64, ok bool) {
	high = minF(minF(a.High(), b.High()), c.High())
	low = maxF(maxF(a.Low(), b.Low()), c.Low())
	if high > low {
		return high, low, true
	}
	return 0, 0, false
}

// biIntersects 笔 b 是否与区间 [low, high] 有任何交集。
//
// 完全脱离的判定（用 "且" 而不是"或"）：
//
//	b.Low > high   → 整笔在区间上方
//	b.High < low   → 整笔在区间下方
//
// 这两个都成立才叫"无交集"，否则就是穿越/在内/触边都算延续。
func biIntersects(b Bi, high, low float64) bool {
	return !(b.Low() > high || b.High() < low)
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
