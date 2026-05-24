package backtest

import (
	"encoding/csv"
	"fmt"
	"os"
)

// WriteTradesCSV 把成交明细写到 CSV 文件。便于事后用表格工具查看。
func WriteTradesCSV(path string, trades []Trade) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"time", "side", "price", "size", "fee", "pnl", "pnl_pct", "equity_after", "reason"}); err != nil {
		return err
	}
	for _, t := range trades {
		row := []string{
			t.Time.Format("2006-01-02 15:04:05"),
			string(t.Side),
			fmt.Sprintf("%.4f", t.Price),
			fmt.Sprintf("%.6f", t.Size),
			fmt.Sprintf("%.4f", t.Fee),
			fmt.Sprintf("%.4f", t.PnL),
			fmt.Sprintf("%.4f", t.PnLPct),
			fmt.Sprintf("%.4f", t.EquityAfter),
			t.Reason,
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// WriteEquityCurveCSV 把权益曲线写到 CSV，每行一个时间点。
//
// 字段：time / equity / price / bh_equity（同期买入并持有的权益）
// bh_equity 用第一根 close 折算成单位 base 数量，再乘以当前 price，方便对比。
func WriteEquityCurveCSV(path string, curve []EquityPoint, initEquity, feeRate float64) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create csv: %w", err)
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	if err := w.Write([]string{"time", "equity", "price", "bh_equity"}); err != nil {
		return err
	}
	if len(curve) == 0 {
		return nil
	}
	// 买入并持有的"持币量"（含一次开仓手续费）
	startPrice := curve[0].Price
	bhQty := initEquity / (startPrice * (1 + feeRate))
	for _, p := range curve {
		bhEquity := bhQty * p.Price
		row := []string{
			p.Time.Format("2006-01-02 15:04:05"),
			fmt.Sprintf("%.4f", p.Equity),
			fmt.Sprintf("%.4f", p.Price),
			fmt.Sprintf("%.4f", bhEquity),
		}
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}
