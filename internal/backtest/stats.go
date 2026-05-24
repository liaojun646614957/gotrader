package backtest

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kraus/gotrader/internal/types"
)

// Stats 回测结果摘要。
type Stats struct {
	InstID        string
	Bar           string
	StartTime     time.Time
	EndTime       time.Time
	Bars          int
	InitEquity    float64
	FinalEquity   float64
	NetPnL        float64
	NetPnLPct     float64
	BuyAndHoldPct float64 // 同期"买入并持有"收益率，作为基线

	TotalTrades  int
	Closes       int // 平仓笔数（带 PnL 的成交，含反转单的平仓部分）
	Wins         int
	Losses       int
	WinRate      float64
	AvgWin       float64
	AvgLoss      float64
	ProfitFactor float64 // 总盈利 / 总亏损（绝对值）
	GrossProfit  float64
	GrossLoss    float64

	MaxDrawdown    float64
	MaxDrawdownPct float64

	TotalFee float64

	// 风险调整后收益指标（学术圈通用）
	BarsPerYear   float64 // 由 equityCurve 时间戳推算（5m=105120, 1H=8760, 1D=365 等）
	AnnualReturn  float64 // 年化收益率
	AnnualVol     float64 // 年化波动率
	SharpeRatio   float64 // 年化 Sharpe（假设无风险利率 = 0）
	SortinoRatio  float64 // 只惩罚下行波动，更贴近"亏钱才是真痛"的直觉
	CalmarRatio   float64 // 年化收益 / 最大回撤百分比（衡量"愿意忍受多大回撤换收益"）
}

// summarize 在 Run 结束时一次性计算所有指标。
func (r *Runner) summarize(klines []types.Kline) *Stats {
	s := &Stats{
		InstID:     r.stratCfg.InstID,
		Bar:        r.stratCfg.Bar,
		Bars:       len(klines),
		InitEquity: r.cfg.InitEquity,
		StartTime:  time.UnixMilli(klines[0].Timestamp),
		EndTime:    time.UnixMilli(klines[len(klines)-1].Timestamp),
	}

	// 最终权益按最后一根 close 估（用统一的 equity() 公式，多空通用）
	last := klines[len(klines)-1]
	s.FinalEquity = r.equity(last.Close)
	s.NetPnL = s.FinalEquity - s.InitEquity
	s.NetPnLPct = s.NetPnL / s.InitEquity

	// Buy & Hold 基线：用第一根 open 全仓买入，最后一根 close 估值（含两次手续费）
	bhBuyPrice := klines[0].Open
	bhSellPrice := last.Close
	bhQty := s.InitEquity / (bhBuyPrice * (1 + r.cfg.FeeRate))
	bhFinal := bhQty * bhSellPrice * (1 - r.cfg.FeeRate)
	s.BuyAndHoldPct = (bhFinal - s.InitEquity) / s.InitEquity

	s.TotalTrades = len(r.trades)
	for _, t := range r.trades {
		s.TotalFee += t.Fee
		// 多空通用判定：成交里只要带 PnL 就算"含平仓"。
		// 反转单（平多+开空）也会带 PnL，但只反映平多的部分。
		if t.PnL != 0 {
			s.Closes++
			if t.PnL > 0 {
				s.Wins++
				s.GrossProfit += t.PnL
			} else {
				s.Losses++
				s.GrossLoss += -t.PnL
			}
		}
	}
	if s.Closes > 0 {
		s.WinRate = float64(s.Wins) / float64(s.Closes)
	}
	if s.Wins > 0 {
		s.AvgWin = s.GrossProfit / float64(s.Wins)
	}
	if s.Losses > 0 {
		s.AvgLoss = s.GrossLoss / float64(s.Losses)
	}
	if s.GrossLoss > 0 {
		s.ProfitFactor = s.GrossProfit / s.GrossLoss
	}

	s.MaxDrawdown, s.MaxDrawdownPct = maxDrawdown(r.equityCurve)

	// 风险调整指标：用 equityCurve 算每根 K 线的对数收益
	s.BarsPerYear = inferBarsPerYear(r.equityCurve)
	s.AnnualReturn, s.AnnualVol, s.SharpeRatio, s.SortinoRatio =
		riskAdjusted(r.equityCurve, s.BarsPerYear)
	if s.MaxDrawdownPct > 0 {
		s.CalmarRatio = s.AnnualReturn / s.MaxDrawdownPct
	}
	return s
}

// inferBarsPerYear 从 equityCurve 的时间戳算每年多少根 K 线。
//
// 这样不需要让调用者配置 bars_per_year，5m / 1H / 1D 都自动对。
// 至少需要 2 个点，否则返回 365 作兜底（不会爆，但 Sharpe 会不准）。
func inferBarsPerYear(curve []EquityPoint) float64 {
	if len(curve) < 2 {
		return 365
	}
	totalSec := curve[len(curve)-1].Time.Sub(curve[0].Time).Seconds()
	if totalSec <= 0 {
		return 365
	}
	bars := float64(len(curve) - 1)
	secPerBar := totalSec / bars
	return (365 * 24 * 3600) / secPerBar
}

// riskAdjusted 算年化收益、年化波动、Sharpe、Sortino。
//
// 公式：
//
//	r_i      = log(eq_i / eq_{i-1})       对数收益（兼容负权益）
//	annRet   = mean(r) * barsPerYear
//	annVol   = std(r)  * sqrt(barsPerYear)
//	Sharpe   = annRet / annVol            假设无风险利率 0
//	Sortino  = annRet / annDownsideVol    annDownsideVol 只算 r<0 的波动
//
// 用对数收益（而不是简单收益）是因为可加性，且回测里权益偶尔会非常波动。
func riskAdjusted(curve []EquityPoint, barsPerYear float64) (annRet, annVol, sharpe, sortino float64) {
	if len(curve) < 2 || barsPerYear <= 0 {
		return
	}
	rets := make([]float64, 0, len(curve)-1)
	for i := 1; i < len(curve); i++ {
		prev, cur := curve[i-1].Equity, curve[i].Equity
		if prev <= 0 || cur <= 0 {
			rets = append(rets, 0) // 权益变负是异常，跳过
			continue
		}
		rets = append(rets, math.Log(cur/prev))
	}
	if len(rets) == 0 {
		return
	}

	// 均值
	var sum float64
	for _, r := range rets {
		sum += r
	}
	mean := sum / float64(len(rets))

	// 全样本标准差（除以 n-1，与多数统计软件一致）
	var sumSq, downSumSq float64
	var downCount int
	for _, r := range rets {
		d := r - mean
		sumSq += d * d
		if r < 0 {
			downSumSq += r * r // Sortino 用绝对负收益的平方（不是相对均值的偏差）
			downCount++
		}
	}
	if len(rets) < 2 {
		return
	}
	std := math.Sqrt(sumSq / float64(len(rets)-1))

	annRet = mean * barsPerYear
	annVol = std * math.Sqrt(barsPerYear)
	if annVol > 0 {
		sharpe = annRet / annVol
	}
	if downCount > 0 {
		downStd := math.Sqrt(downSumSq/float64(downCount)) * math.Sqrt(barsPerYear)
		if downStd > 0 {
			sortino = annRet / downStd
		}
	}
	return
}

// maxDrawdown 计算权益曲线的最大回撤（绝对值 + 占峰值百分比）。
//
// 经典 O(N) 单次扫描：维护历史峰值，回撤 = peak - cur。
func maxDrawdown(curve []EquityPoint) (abs, pct float64) {
	if len(curve) == 0 {
		return 0, 0
	}
	peak := curve[0].Equity
	for _, p := range curve {
		v := p.Equity
		if v > peak {
			peak = v
		}
		dd := peak - v
		if dd > abs {
			abs = dd
			if peak > 0 {
				pct = dd / peak
			}
		}
	}
	return
}

// Format 用人类友好的格式打印报表。
func (s *Stats) Format() string {
	var b strings.Builder
	w := func(format string, args ...interface{}) {
		b.WriteString(fmt.Sprintf(format, args...))
		b.WriteByte('\n')
	}

	w("======== 回测结果 ========")
	w("标的:           %s  周期: %s", s.InstID, s.Bar)
	w("时间:           %s  ->  %s",
		s.StartTime.Format("2006-01-02 15:04"),
		s.EndTime.Format("2006-01-02 15:04"))
	w("K 线根数:       %d", s.Bars)
	w("")
	w("初始资金:       %.2f", s.InitEquity)
	w("结束权益:       %.2f", s.FinalEquity)
	w("净盈亏:         %+.2f (%+.2f%%)", s.NetPnL, s.NetPnLPct*100)
	w("买入并持有基线: %+.2f%%", s.BuyAndHoldPct*100)
	w("超额收益:       %+.2f%%", (s.NetPnLPct-s.BuyAndHoldPct)*100)
	w("")
	w("总成交:         %d", s.TotalTrades)
	w("完整平仓:       %d", s.Closes)
	w("胜 / 负:        %d / %d", s.Wins, s.Losses)
	w("胜率:           %.2f%%", s.WinRate*100)
	w("均盈 / 均亏:    %.2f / %.2f", s.AvgWin, s.AvgLoss)
	w("盈亏比 (PF):    %.2f", s.ProfitFactor)
	w("总手续费:       %.2f", s.TotalFee)
	w("")
	w("最大回撤:       %.2f (%.2f%%)", s.MaxDrawdown, s.MaxDrawdownPct*100)
	w("")
	w("---- 风险调整后指标 (bars/year=%.0f) ----", s.BarsPerYear)
	w("年化收益率:     %+.2f%%", s.AnnualReturn*100)
	w("年化波动率:     %.2f%%", s.AnnualVol*100)
	w("Sharpe Ratio:   %.3f", s.SharpeRatio)
	w("Sortino Ratio:  %.3f", s.SortinoRatio)
	w("Calmar Ratio:   %.3f", s.CalmarRatio)
	w("===========================")
	return b.String()
}
