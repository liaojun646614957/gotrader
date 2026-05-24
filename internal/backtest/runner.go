// Package backtest 在历史 K 线上回放 strategy.Strategy，统计收益与风险。
//
// 设计取舍：
//   - 复用 strategy.Strategy 接口，不为回测重写策略。
//     现有 chan_bs 和未来新加的策略都能直接回测，零成本。
//   - 单标的、现货模式、市价单。多标的、合约、限价单 = V2。
//   - 撮合：信号在 K[i] 产生 → K[i+1] 开盘价成交。不开"未来函数"后门。
//   - 不模拟滑点。手续费用配置费率扣除。
//
// 数据流：
//
//	klines[i] ──► strat.OnKline ──► signals chan ──► executor.fillAt(klines[i+1].Open)
//	                                                          │
//	                                  ┌───────────────────────┘
//	                                  ▼
//	                        strat.OnOrderUpdate(filled)
//	                                  │
//	                                  ▼
//	                            更新持仓/权益
package backtest

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

// Config 回测参数。
type Config struct {
	FeeRate    float64 // 手续费率，单边，例如 0.001 = 0.1%
	InitEquity float64 // 初始资金（USDT 计）
	// Leverage 自动全仓模式的杠杆倍数。
	//   - 0 = 关闭自动全仓，按 signal.Size 字面下单
	//   - >0 时：若 signal.Size==0，runner 自动算实际 qty = equity * leverage / price
	//
	// 注意：本字段只影响"开仓那一笔的 size 计算"，不模拟保证金占用，
	// 也不模拟爆仓。回测里 cash 可以变负数，等于"你已经亏穿了"。
	Leverage float64
	// StopLossPct 单笔止损比例（按"价格反向波动"计算）。
	// 例如 0.03 表示多头价格下跌 3% / 空头价格上涨 3% 触发强平。
	// 0 = 禁用。
	StopLossPct float64
	// MaxDrawdownPct 账户最大回撤上限（相对历史峰值权益）。
	// 触发后停止新开仓（已有仓位等自然平仓或止损）。
	// 0 = 禁用。
	MaxDrawdownPct float64
	Verbose        bool   // 是否打印每笔成交
	TradesCSV      string // 非空则把成交明细写到这个文件
}

// Trade 一次完整的成交（开仓或平仓）。
type Trade struct {
	Time      time.Time
	Side      types.Side
	Price     float64
	Size      float64
	Fee       float64
	Reason    string
	// PnL：仅平仓填，表示这笔平仓相对开仓的实现盈亏（已扣手续费）。
	PnL          float64
	PnLPct       float64
	EquityAfter  float64 // 成交后总权益快照
}

// Runner 单次回测的状态机。线程不安全，由单 goroutine 跑。
//
// 持仓模型（多空统一）：
//
//	qty > 0  → 多头
//	qty < 0  → 空头（仅 SWAP/FUTURES 允许；SPOT 模式 runner 会拦截负 qty）
//	qty == 0 → 空仓
//
// PnL 计算永远是 (price - avgPrice) * qty——这就是"消除特殊情况"，
// 空头时 qty 为负，价格下跌 (price - avgPrice) 为负，乘起来仍然正 = 赚钱。
type Runner struct {
	cfg      Config
	stratCfg strategy.Config
	strat    strategy.Strategy

	allowShort bool // 由 stratCfg.InstType 决定：SWAP/FUTURES = true

	signals chan types.Signal

	// 账户状态
	cash     float64 // 账户权益（USDT 计；含已实现 PnL，不含浮盈）
	qty      float64 // signed 持仓数量
	avgPrice float64 // 当前持仓均价（无论多空都用同一字段）

	// 待执行信号：本根 K 线产生，下一根开盘成交
	pending []types.Signal

	// 风控状态
	peakEquity float64 // 历史最高权益（用于最大回撤判定）
	stopped    bool    // 触发 MaxDrawdown 后置 true，不再开新仓

	// 历史
	trades      []Trade
	equityCurve []EquityPoint
}

// EquityPoint 权益曲线上的一个点。每根 K 线收盘后采样。
type EquityPoint struct {
	Time   time.Time
	Equity float64
	// Price 该时刻标的价格（按 K 线 close）。
	// 用来在同一张图上画"买入并持有"基线。
	Price float64
}

// NewRunner 构造。strat 必须已 Init 完成；Init 在内部完成，避免调用方踩坑。
func NewRunner(cfg Config, stratCfg strategy.Config, strat strategy.Strategy) (*Runner, error) {
	if cfg.InitEquity <= 0 {
		cfg.InitEquity = 10_000
	}
	r := &Runner{
		cfg:        cfg,
		stratCfg:   stratCfg,
		strat:      strat,
		allowShort: stratCfg.InstType == types.InstSwap || stratCfg.InstType == types.InstFutures,
		signals:    make(chan types.Signal, 256),
		cash:       cfg.InitEquity,
		peakEquity: cfg.InitEquity,
	}
	if err := strat.Init(stratCfg, r.signals); err != nil {
		return nil, fmt.Errorf("strategy init: %w", err)
	}
	return r, nil
}

// Run 把 klines 顺序喂给策略，返回统计结果。klines 必须按时间正序。
//
// 流程（每根 K 线）：
//  1. 先用本根开盘价撮合上一根产生的 pending 信号（这是关键时序：先成交、再喂新数据）。
//  2. strat.OnKline(k) → 策略可能产生新信号 → 入 pending（不立即成交）。
//  3. 把权益快照入 equityCurve。
func (r *Runner) Run(klines []types.Kline) (*Stats, error) {
	if len(klines) == 0 {
		return nil, fmt.Errorf("empty klines")
	}

	// 预热：strategy.WarmupBars() 数量的 K 线只喂数据，丢弃信号
	warm := r.strat.WarmupBars()
	if warm > len(klines)-1 {
		warm = len(klines) - 1
	}
	for i := 0; i < warm; i++ {
		r.strat.OnKline(klines[i])
		r.drainSignals() // 预热期信号丢弃
	}
	// 预热完成后，pending 必为空（成交在 Run 主循环里才发生）

	for i := warm; i < len(klines); i++ {
		k := klines[i]

		// 1) 撮合上一根产生的 pending 信号，按本根 open 成交
		r.execPending(k)

		// 2) 本根 K 线区间内检查止损（用 K 高低点判定是否触及止损价）
		r.checkStopLoss(k)

		// 3) 风控停机：超过最大回撤后丢弃后续策略信号
		//    但不取消已有持仓——让它们靠止损或反向信号自然平仓
		if !r.stopped {
			r.strat.OnKline(k)
			r.collectSignals()
		} else {
			r.strat.OnKline(k) // 仍喂数据维持策略内部状态
			r.drainSignals()   // 直接丢弃信号
		}

		// 4) 权益快照（按本根 close 估值）
		eq := r.equity(k.Close)
		if eq > r.peakEquity {
			r.peakEquity = eq
		}
		// 检查最大回撤
		if r.cfg.MaxDrawdownPct > 0 && !r.stopped && r.peakEquity > 0 {
			dd := (r.peakEquity - eq) / r.peakEquity
			if dd >= r.cfg.MaxDrawdownPct {
				r.stopped = true
				slog.Warn("最大回撤触发，停止开新仓",
					"drawdown", dd, "peak", r.peakEquity, "current", eq)
			}
		}
		r.equityCurve = append(r.equityCurve, EquityPoint{
			Time:   time.UnixMilli(k.Timestamp),
			Equity: eq,
			Price:  k.Close,
		})
	}

	return r.summarize(klines), nil
}

// collectSignals 非阻塞抽干 signals chan，加入 pending。
func (r *Runner) collectSignals() {
	for {
		select {
		case sig := <-r.signals:
			r.pending = append(r.pending, sig)
		default:
			return
		}
	}
}

func (r *Runner) drainSignals() int {
	n := 0
	for {
		select {
		case <-r.signals:
			n++
		default:
			return n
		}
	}
}

// checkStopLoss 在本根 K 线开盘价撮合之后调用，检查持仓是否触发止损。
//
// 判定规则（基于 K 线 High/Low 是否触及止损价）：
//
//	多头：avgPrice * (1 - stopLossPct) >= K.Low  → 触发
//	空头：avgPrice * (1 + stopLossPct) <= K.High → 触发
//
// 成交价取"止损价"本身（更接近现实：止损单是触发即市价单，成交在止损价附近）。
// 实盘里会有滑点，这里简化忽略。
func (r *Runner) checkStopLoss(k types.Kline) {
	if r.cfg.StopLossPct <= 0 || r.qty == 0 {
		return
	}
	var stopPrice float64
	var triggered bool
	if r.qty > 0 {
		stopPrice = r.avgPrice * (1 - r.cfg.StopLossPct)
		triggered = k.Low <= stopPrice
	} else {
		stopPrice = r.avgPrice * (1 + r.cfg.StopLossPct)
		triggered = k.High >= stopPrice
	}
	if !triggered {
		return
	}

	// 构造一个 reduce-only 强平信号，按止损价成交（不是 K.Open，这点和正常 fill 不同）
	side := types.SideSell
	if r.qty < 0 {
		side = types.SideBuy
	}
	sig := types.Signal{
		InstID:     r.stratCfg.InstID,
		InstType:   r.stratCfg.InstType,
		Side:       side,
		Type:       types.OrderMarket,
		Size:       absF(r.qty),
		ReduceOnly: true,
		Reason:     "stop_loss",
	}
	slog.Info("止损触发",
		"side", side,
		"qty", r.qty,
		"avgPrice", r.avgPrice,
		"stopPrice", stopPrice,
		"k_low", k.Low,
		"k_high", k.High,
	)
	// 用伪 K 线让 fillOne 在止损价成交
	fakeK := k
	fakeK.Open = stopPrice
	r.fillOne(sig, fakeK)
}

// execPending 撮合 pending 信号。按 K 线 open 价成交。
func (r *Runner) execPending(k types.Kline) {
	if len(r.pending) == 0 {
		return
	}
	for _, sig := range r.pending {
		r.fillOne(sig, k)
	}
	r.pending = r.pending[:0]
}

// fillOne 按统一 delta 模型处理一个信号。
//
// 信号语义（多空合一）：
//
//	Side=Buy,  ReduceOnly=false → 目标方向 = 多
//	Side=Sell, ReduceOnly=false → 目标方向 = 空（SPOT 模式下视为"平多到 0"）
//	ReduceOnly=true             → 仅平掉当前仓（不开反向）
//
// 实际执行 = 把 qty 从当前推到目标，跨过零点的部分实现 PnL，
// 留在新方向的部分按当前价开仓。空仓后 avgPrice 重置为 0。
//
// 这个函数取代了原本 SideBuy / SideSell 两段重复逻辑。多空、开平反转
// 全部走同一条路径，没有 if-else 分叉。
func (r *Runner) fillOne(sig types.Signal, k types.Kline) {
	price := k.Open

	// 1) 算"目标 qty"
	target := r.targetQty(sig, price)
	delta := target - r.qty
	if nearZero(delta) {
		return // 同状态忽略
	}

	// 2) 跨过 0 点的部分先实现盈亏
	var realized float64
	var closedAbs float64 // 平掉的"绝对仓位"，用于算手续费 + 计入"完整平仓"统计
	closing := r.qty != 0 && sameSign(r.qty, delta) == false
	if closing {
		// 被平掉的"持仓段"：跟 r.qty 同号，绝对值不超过两者较小。
		// PnL = (price - avgPrice) * qty_closed —— 多空通用公式。
		//   多头 qty=+1 avg=100 price=120：pnl = (120-100)*(+1) = +20
		//   空头 qty=-1 avg=100 price=80：pnl = (80-100)*(-1) = +20
		qtyClosed := minAbs(r.qty, delta)
		realized = (price - r.avgPrice) * qtyClosed
		closedAbs = absF(qtyClosed)
		// 把 qty 往 0 推：qty=+1 平 +1 → qty -= +1 → 0；qty=-1 平 -1 → qty -= -1 → 0
		r.qty -= qtyClosed
		if nearZero(r.qty) {
			r.qty = 0
			r.avgPrice = 0
		}
	}

	// 3) 剩余 delta 用于开/加仓
	remaining := target - r.qty
	openedAbs := absF(remaining)
	if !nearZero(remaining) {
		if r.qty == 0 {
			r.avgPrice = price
		} else if sameSign(r.qty, remaining) {
			// 加仓：加权平均
			r.avgPrice = (r.avgPrice*absF(r.qty) + price*openedAbs) / (absF(r.qty) + openedAbs)
		}
		r.qty += remaining
	}

	// 4) 手续费按"成交的绝对张数 * 价格"计算（开仓和平仓都收）
	fee := (closedAbs + openedAbs) * price * r.cfg.FeeRate

	// 5) 现金变化：合约模型下 cash = 账户权益（已实现）
	//    现货模型下 cash 也是同样含义（之前的"持有 base + cash"模型在这里被统一）
	r.cash += realized - fee

	// 6) 记录成交
	t := Trade{
		Time:        time.UnixMilli(k.Timestamp),
		Side:        sig.Side,
		Price:       price,
		Size:        closedAbs + openedAbs,
		Fee:         fee,
		Reason:      sig.Reason,
		EquityAfter: r.equity(price),
	}
	if closedAbs > 0 {
		// 只有平仓才计 PnL（一笔可能"反转"=同时含平+开，PnL 也只记平的部分）
		t.PnL = realized - fee*(closedAbs/(closedAbs+openedAbs))
		if r.avgPrice > 0 || closedAbs > 0 {
			// 用原成本作为基准计算百分比
			cost := closedAbs * (r.avgPrice + 0) // 之后 avg 可能改，这里粗略
			if cost > 0 {
				t.PnLPct = t.PnL / cost
			}
		}
	}
	r.trades = append(r.trades, t)
	r.notify(sig, types.StatusFilled, t.Size, price, t)
}

// targetQty 把 signal 翻译成"目标 signed qty"。
//
// 设计要点：
//   - SPOT 模式不允许 short：Sell 信号一律视为"目标=0"（即"平多到空仓"）
//   - ReduceOnly=true：目标永远是 0（不管 Side 是什么）
//   - Size==0 + Leverage>0：自动全仓模式，size 由 runner 按当前权益计算
//   - 否则按 Side: Buy=+size, Sell=-size
//
// price 是当前 K 线开盘价，自动全仓时用它来折算 qty。
func (r *Runner) targetQty(sig types.Signal, price float64) float64 {
	if sig.ReduceOnly {
		return 0
	}

	// 决定实际下单的"基础币数量"
	size := sig.Size
	if size <= 0 && r.cfg.Leverage > 0 && price > 0 {
		// 自动全仓：用当前权益 + 杠杆决定仓位
		// 留 1% 给手续费缓冲，避免精确等于"满仓"时被手续费踢爆
		size = r.equity(price) * r.cfg.Leverage / price * 0.99
	}
	if size <= 0 {
		return r.qty // 无效信号，保持现状
	}

	switch sig.Side {
	case types.SideBuy:
		return size
	case types.SideSell:
		if r.allowShort {
			return -size
		}
		return 0
	}
	return r.qty
}

// equity 当前账户总权益 = cash + 持仓浮盈/浮亏。
//
// 浮盈 = (markPrice - avgPrice) * qty。空头时 qty < 0，价格下跌浮盈仍为正。
// 这个公式同时适用于现货和合约——前者历史代码本质等价（cash 是减完成本后的余额，
// 加上持仓估值就是当前权益）。
func (r *Runner) equity(markPrice float64) float64 {
	if r.qty == 0 {
		return r.cash
	}
	return r.cash + (markPrice-r.avgPrice)*r.qty
}

// ---- 数值小工具 ----

func nearZero(x float64) bool { return absF(x) < 1e-12 }
func absF(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
func signOf(x float64) float64 {
	if x > 0 {
		return 1
	}
	if x < 0 {
		return -1
	}
	return 0
}
func sameSign(a, b float64) bool { return signOf(a) == signOf(b) && a != 0 && b != 0 }

// minAbs 返回 "和 a 同号、绝对值不超过 a 也不超过 b" 的值。
// 例如：a=+3, b=-5 → +3（要从 a 里扣 3 个单位）
//
//	a=-7, b=+4 → -4
func minAbs(a, b float64) float64 {
	if absF(a) <= absF(b) {
		return a
	}
	// a 比 b 大，所以扣 |b|，方向跟 a 一致
	if a > 0 {
		return absF(b)
	}
	return -absF(b)
}

// notify 通知策略订单成交。回测里订单是"瞬时填满"，构造一个 filled Order 回调。
func (r *Runner) notify(sig types.Signal, status types.OrderStatus, filled, avgPx float64, t Trade) {
	if r.cfg.Verbose {
		slog.Info("回测成交",
			"time", t.Time.Format("2006-01-02 15:04"),
			"side", sig.Side,
			"price", avgPx,
			"size", filled,
			"fee", t.Fee,
			"pnl", t.PnL,
			"equity", t.EquityAfter,
			"reason", sig.Reason,
		)
	}
	r.strat.OnOrderUpdate(types.Order{
		InstID:     sig.InstID,
		InstType:   sig.InstType,
		Side:       sig.Side,
		PosSide:    sig.PosSide,
		Type:       sig.Type,
		Price:      avgPx,
		Size:       filled,
		ReduceOnly: sig.ReduceOnly,
		Reason:     sig.Reason,
		Status:     status,
		FilledSize: filled,
		AvgPrice:   avgPx,
		CreatedAt:  t.Time,
		UpdatedAt:  t.Time,
	})
}

// Trades 返回所有成交记录（只读）。
func (r *Runner) Trades() []Trade { return r.trades }

// EquityCurve 返回权益曲线（每根 K 线一个点）。
func (r *Runner) EquityCurve() []EquityPoint { return r.equityCurve }
