package chanlun

// detectFractals 增量识别 MergedKline 序列尾部的分型。
//
// 规则：
//
//	设三根连续 MergedKline 为 a, b, c（b 在中间）。
//	  - 顶分型：b.High > a.High 且 b.High > c.High（且 b.Low > a.Low, b.Low > c.Low，
//	            由"相邻 MK 无包含"自然保证）。
//	  - 底分型：b.High < a.High 且 b.High < c.High（同理底也成立）。
//
// 确认机制：分型默认 Confirmed=false。当其右侧再走出 FractalRightConfirm 根独立 MK
// （即 mks 长度从 b 算起再 +N+1 且未破坏极值），就标记为已确认。
//
// 此函数每次只看尾部新增的部分，幂等。
func (e *Engine) detectFractals() {
	n := len(e.mks)
	// 至少 3 根才有"中间根"，但只看最新可识别的中间根：idx = n-2
	if n >= 3 {
		mid := n - 2
		if fx, ok := classifyFractal(e.mks, mid); ok {
			// 去重：若 fxs 末尾就是同一个 MKIdx，跳过
			if k := len(e.fxs); k == 0 || e.fxs[k-1].MKIdx != mid {
				e.fxs = append(e.fxs, fx)
			}
		}
	}

	// 更新已有分型的 Confirmed 标志。
	// 规则：右侧至少 FractalRightConfirm 根独立 MK，且这些 MK 未破坏分型极值。
	for i := range e.fxs {
		if e.fxs[i].Confirmed {
			continue
		}
		f := &e.fxs[i]
		right := n - 1 - f.MKIdx
		if right < e.cfg.FractalRightConfirm+1 {
			continue
		}
		// 检查右侧 K 线是否破坏极值
		broken := false
		for j := f.MKIdx + 1; j < n; j++ {
			switch f.Dir {
			case DirUp:
				if e.mks[j].High > f.Price {
					broken = true
				}
			case DirDown:
				if e.mks[j].Low < f.Price {
					broken = true
				}
			}
			if broken {
				break
			}
		}
		if !broken {
			f.Confirmed = true
		}
	}
}

// classifyFractal 看 mks[idx] 是否构成顶/底分型（idx 必须 >=1 且 < len-1）。
func classifyFractal(mks []MergedKline, idx int) (Fractal, bool) {
	if idx <= 0 || idx >= len(mks)-1 {
		return Fractal{}, false
	}
	a, b, c := mks[idx-1], mks[idx], mks[idx+1]
	switch {
	case b.High > a.High && b.High > c.High:
		// 顶分型：依赖"相邻 MK 无包含"，所以 b.Low > a.Low 且 b.Low > c.Low 自动满足
		return Fractal{Dir: DirUp, Price: b.High, MKIdx: idx}, true
	case b.Low < a.Low && b.Low < c.Low:
		return Fractal{Dir: DirDown, Price: b.Low, MKIdx: idx}, true
	}
	return Fractal{}, false
}
