package chanlun

// macd 标准 MACD 指标的增量计算器。
//
// 公式（与主流软件一致）：
//
//	EMA_fast = EMA(close, fast)
//	EMA_slow = EMA(close, slow)
//	DIF      = EMA_fast - EMA_slow
//	DEA      = EMA(DIF, signal)
//	HIST     = 2 * (DIF - DEA)
//
// 这里 DIF/DEA/HIST 序列与喂入的 close 序列等长（含预热期间的"未稳定"值）。
// 调用方应在 long+signal 根之后才信任输出。
type macd struct {
	fast, slow, signal int
	emaFast, emaSlow   *ema
	emaDIF             *ema

	dif  []float64
	dea  []float64
	hist []float64
}

func newMACD(fast, slow, signal int) *macd {
	return &macd{
		fast:    fast,
		slow:    slow,
		signal:  signal,
		emaFast: newEMA(fast),
		emaSlow: newEMA(slow),
		emaDIF:  newEMA(signal),
	}
}

// Feed 喂一个收盘价，输出当前最新的 DIF/DEA/HIST。
func (m *macd) Feed(close float64) (dif, dea, hist float64) {
	ef := m.emaFast.Feed(close)
	es := m.emaSlow.Feed(close)
	dif = ef - es
	dea = m.emaDIF.Feed(dif)
	hist = 2 * (dif - dea)

	m.dif = append(m.dif, dif)
	m.dea = append(m.dea, dea)
	m.hist = append(m.hist, hist)
	return
}

// HistSlice 返回 [from, to] 闭区间内的 HIST 切片副本。
// 越界自动 clamp。
func (m *macd) HistSlice(from, to int) []float64 {
	if from < 0 {
		from = 0
	}
	if to >= len(m.hist) {
		to = len(m.hist) - 1
	}
	if from > to {
		return nil
	}
	out := make([]float64, to-from+1)
	copy(out, m.hist[from:to+1])
	return out
}

// AreaInRange 计算 [from, to] 闭区间内 HIST 的"同向面积"。
//
// dir=DirUp 取 hist>0 的累加，DirDown 取 hist<0 的累加绝对值。
// 用于"背驰"比较：同向两段笔，后者面积更小 + 价格创新极值 = 背驰。
//
// 用同向过滤（而非全部绝对值）的原因：一段下跌笔中可能短暂出现 hist>0 的弱反弹，
// 那些不该计入下跌动能。
func (m *macd) AreaInRange(from, to int, dir Direction) float64 {
	if from < 0 {
		from = 0
	}
	if to >= len(m.hist) {
		to = len(m.hist) - 1
	}
	if from > to {
		return 0
	}
	var sum float64
	for i := from; i <= to; i++ {
		h := m.hist[i]
		switch dir {
		case DirUp:
			if h > 0 {
				sum += h
			}
		case DirDown:
			if h < 0 {
				sum += -h
			}
		default:
			if h < 0 {
				sum += -h
			} else {
				sum += h
			}
		}
	}
	return sum
}

// Len 返回已喂入的样本数。
func (m *macd) Len() int { return len(m.hist) }

// ema 标准指数平滑均线。
//
// 公式：EMA_t = alpha * x_t + (1 - alpha) * EMA_{t-1}，其中 alpha = 2/(period+1)。
// 首值用 x_0 自身（也可用 SMA(period) 作种子，差别在最初 period 根上）。
type ema struct {
	alpha float64
	value float64
	init  bool
}

func newEMA(period int) *ema {
	return &ema{alpha: 2.0 / float64(period+1)}
}

func (e *ema) Feed(x float64) float64 {
	if !e.init {
		e.value = x
		e.init = true
		return e.value
	}
	e.value = e.alpha*x + (1-e.alpha)*e.value
	return e.value
}
