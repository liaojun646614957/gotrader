// Package chanlun 缠中说缠理论的核心数据处理。
//
// 设计原则：
//   - 纯数据处理，无 IO，无并发，无 channel。调用方喂 K 线，拿信号。
//   - 增量 + 只追加：每根新 K 线只更新最末几个元素，绝不重算整段历史。
//   - 严格区分"已确认"与"潜在"：缠论的分型/笔在 K 线刚走完时会反复变化，
//     未确认状态绝不发信号，否则策略被自己骗。
//
// 数据流（每一层都是上一层的"摘要"）：
//
//	[]Kline → []MergedKline → []Fractal → []Bi → []Pivot
//	                                ↓
//	                             []BSPoint
//	[]MACD 与 []MergedKline 同步
//
// 包名 chanlun 而非 chan（Go 关键字）。
package chanlun

import "github.com/kraus/gotrader/internal/types"

// Direction K 线方向 / 分型方向 / 笔方向。
type Direction int8

const (
	DirNone Direction = 0
	DirUp   Direction = 1  // 向上
	DirDown Direction = -1 // 向下
)

// MergedKline 经过包含关系处理后的 K 线。
//
// 经过合并后，相邻两根 MergedKline 一定**没有包含关系**，
// 即 H_i > H_{i-1} 且 L_i > L_{i-1}（向上），或 H_i < H_{i-1} 且 L_i < L_{i-1}（向下）。
// 这是分型识别能成立的前提。
type MergedKline struct {
	High, Low      float64
	StartTS, EndTS int64
	// StartRawIdx/EndRawIdx 这根 MergedKline 覆盖的原始 K 线下标区间（闭区间）。
	// 用于把笔/中枢映射回原始 K 线，配合 MACD 取面积。
	StartRawIdx, EndRawIdx int
	// Dir 是相对前一根 MergedKline 的方向。第一根 Dir=DirNone。
	// 这个字段在"包含关系处理"时决定向上取高高、向下取低低。
	Dir Direction
}

// Fractal 分型。
//
// 顶分型：连续 3 根 MergedKline，中间那根 High 最高且 Low 也最高。
// 底分型：连续 3 根 MergedKline，中间那根 High 最低且 Low 也最低。
//
// 严格定义还要求中间那根相对左右是"完全的"高/低，缠论原文如此。
type Fractal struct {
	Dir   Direction // DirUp=顶, DirDown=底
	Price float64   // 顶取 High，底取 Low
	MKIdx int       // 在 MergedKlines 切片里的下标（中间那根）
	// Confirmed 表示右侧已走出足够的 K 线且未破坏分型极值。
	// 未确认的分型可能被后续走势改写。
	Confirmed bool
}

// Bi 笔。两端是已确认的异向分型。
//
// 旧笔派规则（更稳）：两个分型之间至少 5 根独立的 MergedKline（含两端所在 MK）。
type Bi struct {
	Dir        Direction // 由 From 到 To 的方向
	From, To   Fractal   // 起止分型（一定异向）
	StartMKIdx int       // From 所在 MergedKline 下标
	EndMKIdx   int       // To 所在 MergedKline 下标
	// MACDArea 该笔覆盖区间内 MACD 柱子绝对值之和，用于背驰比较。
	// 仅在 Confirmed=true 时有效。
	MACDArea  float64
	Confirmed bool // 是否为已确认笔（To 分型已确认）
}

// High 笔的最高价（顶分型价）。
func (b Bi) High() float64 {
	if b.Dir == DirUp {
		return b.To.Price
	}
	return b.From.Price
}

// Low 笔的最低价（底分型价）。
func (b Bi) Low() float64 {
	if b.Dir == DirDown {
		return b.To.Price
	}
	return b.From.Price
}

// Pivot 笔中枢。连续三笔的价格重叠区间。
//
// 区间：High = min(三笔顶), Low = max(三笔底)，要求 High > Low。
// BiIdxs 持有组成该中枢的笔在 Bis() 切片中的下标，至少 3 个。
type Pivot struct {
	High, Low float64
	BiIdxs    []int
	// Dir 中枢之前的趋势方向（即第一笔进入中枢的反方向）。
	// 上涨趋势中的中枢 Dir=DirUp，下跌趋势中 Dir=DirDown。
	Dir       Direction
	Broken    bool // 是否已被破坏（脱离中枢区间且未回归）
}

// BSPoint 买卖点。
//
// Kind: 1=一类买, -1=一类卖, 2=二类买, -2=二类卖
//
// 设计为不可变快照：策略层拿到就用，不要回头改。
// 用 (Kind, BiEndMKIdx) 二元组作为业务主键，便于去重。
type BSPoint struct {
	Kind        int
	MKIdx       int     // 触发时所在 MergedKline 下标（= BiEndMKIdx）
	BiEndMKIdx  int     // 触发该买卖点的笔的 EndMKIdx，用于业务去重
	Price       float64 // 触发价（一类 = 该笔的极值；二类 = 回调笔的极值）
	Reason      string
}

// Engine 缠论计算引擎。线程不安全，由单 goroutine 调用。
//
// 用法：
//
//	e := chanlun.NewEngine(chanlun.Config{})
//	for _, k := range klines {
//	    bsps := e.Feed(k)
//	    for _, bs := range bsps {
//	        // 拿到新产生的买卖点
//	    }
//	}
type Engine struct {
	cfg Config

	// 原始 K 线序列。只用最后一根（用于增量合并）+ 长度（用于赋下标）。
	rawCount int

	mks     []MergedKline
	macd    *macd
	fxs     []Fractal // 已识别的所有分型（含潜在和已确认）
	bis     []Bi      // 已识别的所有笔（含最后一条未确认的）
	pivots  []Pivot   // 已识别的所有笔中枢
	bsps    []BSPoint // 已产生的所有买卖点

	// 分型确认的"右侧 K 线计数"：fxs 最后一个分型已经过了多少根独立 MK。
	// 用于实现"右侧至少 N 根独立 MK 才确认"。
	// 注意：当出现反向分型时，前一同向分型自动确认。
}

// Config 引擎配置。所有字段都有合理默认值。
type Config struct {
	// MACDFast/Slow/Signal 标准参数 12/26/9。
	MACDFast, MACDSlow, MACDSignal int
	// FractalRightConfirm 分型右侧需要多少根独立 MK 才视作确认。默认 1。
	FractalRightConfirm int
	// MinBiMKs 笔的最小 MergedKline 数（含两端），旧笔派 5，新笔派 1。默认 5。
	MinBiMKs int
}

// NewEngine 构造引擎。零值 Config 即可。
func NewEngine(cfg Config) *Engine {
	if cfg.MACDFast <= 0 {
		cfg.MACDFast = 12
	}
	if cfg.MACDSlow <= 0 {
		cfg.MACDSlow = 26
	}
	if cfg.MACDSignal <= 0 {
		cfg.MACDSignal = 9
	}
	if cfg.FractalRightConfirm <= 0 {
		cfg.FractalRightConfirm = 1
	}
	if cfg.MinBiMKs <= 0 {
		cfg.MinBiMKs = 5
	}
	return &Engine{
		cfg:  cfg,
		macd: newMACD(cfg.MACDFast, cfg.MACDSlow, cfg.MACDSignal),
	}
}

// Feed 喂一根 K 线。返回此次新产生的买卖点（通常 0~1 个）。
//
// K 线必须按时间正序、不能重复喂同一根。
func (e *Engine) Feed(k types.Kline) []BSPoint {
	e.rawCount++
	e.mergeKline(k)
	e.detectFractals()
	e.buildBis()
	e.updatePivots()
	return e.detectBSPoints()
}

// MergedKlines 返回当前所有合并 K 线（只读）。
func (e *Engine) MergedKlines() []MergedKline { return e.mks }

// Fractals 返回当前所有分型（只读）。
func (e *Engine) Fractals() []Fractal { return e.fxs }

// Bis 返回当前所有笔（只读，包含最后一条未确认笔）。
func (e *Engine) Bis() []Bi { return e.bis }

// Pivots 返回当前所有笔中枢（只读）。
func (e *Engine) Pivots() []Pivot { return e.pivots }

// BSPoints 返回所有已产生的买卖点（只读）。
func (e *Engine) BSPoints() []BSPoint { return e.bsps }

// LastConfirmedBi 返回最近一条已确认的笔。没有则返回 nil。
func (e *Engine) LastConfirmedBi() *Bi {
	for i := len(e.bis) - 1; i >= 0; i-- {
		if e.bis[i].Confirmed {
			return &e.bis[i]
		}
	}
	return nil
}
