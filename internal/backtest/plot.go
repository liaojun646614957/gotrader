package backtest

import (
	"fmt"
	"image/color"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
)

// PlotEquityCurve 把权益曲线画成 PNG 图。
//
// 一张图里两条线：
//   - 策略权益（蓝）：实际跑出来的资金曲线
//   - 买入并持有（灰虚线）：同期"第一根开盘价全仓买入并持到最后"
//   - 初始资金（红虚线）：水平参考线，肉眼看是否盈利
//
// 三条共享同一坐标系，方便一眼看出策略到底有没有跑赢"什么都不做"。
//
// X 轴是 K 线序号（不是时间戳）。这是个工程取舍：
//   - 时间戳轴在分钟级别 + 数千根时会拥挤变丑。
//   - 序号轴朴素但永远清晰。GUI/Excel 端需要看时间的，直接读 CSV 即可。
func PlotEquityCurve(path string, curve []EquityPoint, initEquity, feeRate float64, title string) error {
	if len(curve) == 0 {
		return fmt.Errorf("空权益曲线")
	}

	p := plot.New()
	p.Title.Text = title
	p.X.Label.Text = "Bar Index"
	p.Y.Label.Text = "Equity (USDT)"
	p.Add(plotter.NewGrid())
	// 注意：gonum/plot 默认字体不含中文字形。用英文标签保证任何机器渲染都正常。
	// 时间/标的等中文信息读 CSV 即可，PNG 只承担"一眼看趋势"的视觉职责。

	// 策略权益
	stratLine, err := makeLine(curve, func(i int, ep EquityPoint) float64 { return ep.Equity })
	if err != nil {
		return err
	}
	stratLine.LineStyle.Width = vg.Points(1.5)
	stratLine.LineStyle.Color = color.RGBA{R: 30, G: 100, B: 200, A: 255}

	// 买入并持有基线
	startPrice := curve[0].Price
	bhQty := initEquity / (startPrice * (1 + feeRate))
	bhLine, err := makeLine(curve, func(i int, ep EquityPoint) float64 { return bhQty * ep.Price })
	if err != nil {
		return err
	}
	bhLine.LineStyle.Width = vg.Points(1.0)
	bhLine.LineStyle.Color = color.RGBA{R: 150, G: 150, B: 150, A: 255}
	bhLine.LineStyle.Dashes = []vg.Length{vg.Points(4), vg.Points(2)}

	// 初始资金水平参考线
	initLine, err := plotter.NewLine(plotter.XYs{
		{X: 0, Y: initEquity},
		{X: float64(len(curve) - 1), Y: initEquity},
	})
	if err != nil {
		return err
	}
	initLine.LineStyle.Width = vg.Points(0.5)
	initLine.LineStyle.Color = color.RGBA{R: 200, G: 0, B: 0, A: 180}
	initLine.LineStyle.Dashes = []vg.Length{vg.Points(2), vg.Points(2)}

	p.Add(initLine, bhLine, stratLine)
	p.Legend.Add("Strategy", stratLine)
	p.Legend.Add("Buy & Hold", bhLine)
	p.Legend.Add("Initial Equity", initLine)
	p.Legend.Top = true
	p.Legend.Left = true

	// 1200x600 给个能看清的尺寸
	if err := p.Save(12*vg.Inch, 6*vg.Inch, path); err != nil {
		return fmt.Errorf("save png: %w", err)
	}
	return nil
}

func makeLine(curve []EquityPoint, y func(i int, ep EquityPoint) float64) (*plotter.Line, error) {
	pts := make(plotter.XYs, len(curve))
	for i, ep := range curve {
		pts[i].X = float64(i)
		pts[i].Y = y(i, ep)
	}
	return plotter.NewLine(pts)
}
