package chanlun

import "fmt"

// detectBSPoints 检测新的买卖点。返回本次新增的 BSPoints（通常 0~1 个）。
//
// MVP 只判定第 1 类和第 2 类。
//
// 第 1 类买点（一类买，Kind=1）：
//
//	下跌趋势中，最近一根下跌笔 b_n 与"参考下跌笔" b_{n-2} 比较：
//	  - b_n.To.Price < b_{n-2}.To.Price  （创新低）
//	  - b_n.MACDArea < b_{n-2}.MACDArea  （底背驰：动能减弱）
//	触发价 = b_n.To.Price
//
// 第 1 类卖点对称（顶背驰）。
//
// 第 2 类买点（Kind=2）：
//
//	在已有一类买点 bs1（位于笔 b_{1}.To）之后：
//	  b_{1} = 下跌笔（一类买的笔）
//	  b_{2} = 反弹笔（上涨）
//	  b_{3} = 回调笔（下跌）
//	  若 b_{3}.To.Price > b_{1}.To.Price（不创新低）→ 二类买。
//
// 第 2 类卖点对称。
//
// 去重：用 (Kind, BiEndMKIdx) 业务主键，已存在则不重复加入。
func (e *Engine) detectBSPoints() []BSPoint {
	var newOnes []BSPoint

	// 收集已确认笔（保持原始 e.bis 中的位置无关，这里就是顺序）
	confirmed := make([]int, 0, len(e.bis))
	for i, b := range e.bis {
		if b.Confirmed {
			confirmed = append(confirmed, i)
		}
	}
	if len(confirmed) == 0 {
		return nil
	}

	// 1) 一类买卖点：需要至少 3 根同向同类比较，最少 3 根已确认笔（b_{n-2}, b_{n-1}, b_n）
	if len(confirmed) >= 3 {
		bn := e.bis[confirmed[len(confirmed)-1]]
		bRef := e.bis[confirmed[len(confirmed)-3]]
		if bn.Dir == bRef.Dir {
			if bs, ok := check1stBS(bn, bRef); ok {
				if !e.hasBSP(bs.Kind, bs.BiEndMKIdx) {
					e.bsps = append(e.bsps, bs)
					newOnes = append(newOnes, bs)
				}
			}
		}
	}

	// 2) 二类买卖点：在最近一个一类点之后两笔
	if len(confirmed) >= 3 && len(e.bsps) > 0 {
		bs1 := findLast1stBS(e.bsps)
		if bs1 != nil {
			if bs, ok := e.check2ndBS(*bs1, confirmed); ok {
				if !e.hasBSP(bs.Kind, bs.BiEndMKIdx) {
					e.bsps = append(e.bsps, bs)
					newOnes = append(newOnes, bs)
				}
			}
		}
	}

	return newOnes
}

// check1stBS 判定一类买/卖点：当前笔 bn 与参考同向笔 bRef 比较。
func check1stBS(bn, bRef Bi) (BSPoint, bool) {
	switch bn.Dir {
	case DirDown:
		// 一类买：创新低 + 底背驰
		if bn.To.Price < bRef.To.Price && bn.MACDArea < bRef.MACDArea && bRef.MACDArea > 0 {
			return BSPoint{
				Kind:       1,
				MKIdx:      bn.EndMKIdx,
				BiEndMKIdx: bn.EndMKIdx,
				Price:      bn.To.Price,
				Reason: fmt.Sprintf("一类买/底背驰: cur_low=%.4f<ref_low=%.4f area=%.2f<%.2f",
					bn.To.Price, bRef.To.Price, bn.MACDArea, bRef.MACDArea),
			}, true
		}
	case DirUp:
		// 一类卖：创新高 + 顶背驰
		if bn.To.Price > bRef.To.Price && bn.MACDArea < bRef.MACDArea && bRef.MACDArea > 0 {
			return BSPoint{
				Kind:       -1,
				MKIdx:      bn.EndMKIdx,
				BiEndMKIdx: bn.EndMKIdx,
				Price:      bn.To.Price,
				Reason: fmt.Sprintf("一类卖/顶背驰: cur_high=%.4f>ref_high=%.4f area=%.2f<%.2f",
					bn.To.Price, bRef.To.Price, bn.MACDArea, bRef.MACDArea),
			}, true
		}
	}
	return BSPoint{}, false
}

// check2ndBS 判定二类买/卖点。
//
// 假设当前最末三笔为 b_{n-2}, b_{n-1}, b_n：
//   - 找最近的一类点 bs1。
//   - 一类买场景：b_{n-2} 应为下跌笔且 b_{n-2}.To 就是 bs1 触发位置；
//     b_{n-1} 上涨，b_n 下跌；b_n.To.Price > bs1.Price → 二类买。
//
// 这里用 EndMKIdx 比对而不是 Price，因为价格可能巧合相等。
func (e *Engine) check2ndBS(bs1 BSPoint, confirmed []int) (BSPoint, bool) {
	if len(confirmed) < 3 {
		return BSPoint{}, false
	}
	bN := e.bis[confirmed[len(confirmed)-1]]
	bN1 := e.bis[confirmed[len(confirmed)-2]]
	bN2 := e.bis[confirmed[len(confirmed)-3]]

	// 二类点的"那一笔"必须是回调笔，方向与一类点的触发笔相同
	// （一类买的触发笔是下跌笔 → 二类买的回调笔也是下跌笔）
	switch bs1.Kind {
	case 1:
		if bN.Dir != DirDown || bN1.Dir != DirUp || bN2.Dir != DirDown {
			return BSPoint{}, false
		}
		// b_{n-2} 必须是一类点对应的那笔
		if bN2.EndMKIdx != bs1.BiEndMKIdx {
			return BSPoint{}, false
		}
		if bN.To.Price > bs1.Price {
			return BSPoint{
				Kind:       2,
				MKIdx:      bN.EndMKIdx,
				BiEndMKIdx: bN.EndMKIdx,
				Price:      bN.To.Price,
				Reason: fmt.Sprintf("二类买/未破一类低: cur_low=%.4f>bs1_low=%.4f",
					bN.To.Price, bs1.Price),
			}, true
		}
	case -1:
		if bN.Dir != DirUp || bN1.Dir != DirDown || bN2.Dir != DirUp {
			return BSPoint{}, false
		}
		if bN2.EndMKIdx != bs1.BiEndMKIdx {
			return BSPoint{}, false
		}
		if bN.To.Price < bs1.Price {
			return BSPoint{
				Kind:       -2,
				MKIdx:      bN.EndMKIdx,
				BiEndMKIdx: bN.EndMKIdx,
				Price:      bN.To.Price,
				Reason: fmt.Sprintf("二类卖/未破一类高: cur_high=%.4f<bs1_high=%.4f",
					bN.To.Price, bs1.Price),
			}, true
		}
	}
	return BSPoint{}, false
}

// findLast1stBS 从买卖点列表末尾向前找最近的一类买/卖点。
func findLast1stBS(bsps []BSPoint) *BSPoint {
	for i := len(bsps) - 1; i >= 0; i-- {
		if bsps[i].Kind == 1 || bsps[i].Kind == -1 {
			return &bsps[i]
		}
	}
	return nil
}

// hasBSP 业务去重：相同 (Kind, BiEndMKIdx) 视为同一买卖点。
func (e *Engine) hasBSP(kind, biEndMKIdx int) bool {
	for _, bs := range e.bsps {
		if bs.Kind == kind && bs.BiEndMKIdx == biEndMKIdx {
			return true
		}
	}
	return false
}
