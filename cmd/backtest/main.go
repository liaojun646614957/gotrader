// gobacktest: 在 OKX 历史 K 线上回放策略，输出绩效报告。
//
// 用法:
//
//	./bin/backtest -config config.yaml
//	./bin/backtest -config config.yaml -bars 2000 -verbose
//
// 数据来源：OKX 公开接口 /api/v5/market/history-candles（无需 API key）。
// 撮合模型：信号在 K[i] 产生 → K[i+1] 开盘价成交。手续费率按 backtest.fee_rate。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/kraus/gotrader/internal/backtest"
	"github.com/kraus/gotrader/internal/config"
	"github.com/kraus/gotrader/internal/exchange/okx"
	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	barsFlag := flag.Int("bars", 0, "K 线总根数（覆盖配置）")
	verboseFlag := flag.Bool("verbose", false, "打印每笔成交")
	tradesCSVFlag := flag.String("trades-csv", "", "成交明细 CSV 输出路径（覆盖配置）")
	equityCSVFlag := flag.String("equity-csv", "", "权益曲线 CSV 输出路径（覆盖配置）")
	equityPNGFlag := flag.String("equity-png", "", "权益曲线 PNG 输出路径（覆盖配置）")
	flag.Parse()

	cfg, err := config.LoadForBacktest(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	setupLogger(cfg.LogLevel)

	// 命令行覆盖配置
	if *barsFlag > 0 {
		cfg.Backtest.Bars = *barsFlag
	}
	if *verboseFlag {
		cfg.Backtest.Verbose = true
	}
	if *tradesCSVFlag != "" {
		cfg.Backtest.TradesCSV = *tradesCSVFlag
	}
	if *equityCSVFlag != "" {
		cfg.Backtest.EquityCSV = *equityCSVFlag
	}
	if *equityPNGFlag != "" {
		cfg.Backtest.EquityPNG = *equityPNGFlag
	}

	// 构造 REST 客户端：API key 留空，只用公开行情。
	rest := okx.NewClient("", "", "", cfg.OKX.RestURL, !cfg.OKX.Live)

	// 解析 EndTime（可选）
	var endTS int64
	if cfg.Backtest.EndTime != "" {
		t, err := time.Parse("2006-01-02", cfg.Backtest.EndTime)
		if err != nil {
			fatal("解析 backtest.end_time 失败（期望 YYYY-MM-DD）: %v", err)
		}
		endTS = t.UnixMilli()
	}

	// 拉历史 K 线
	bar := paramString(cfg.Strategy.Params, "bar", "1m")
	slog.Info("拉取历史 K 线", "instId", cfg.Strategy.InstID, "bar", bar,
		"bars", cfg.Backtest.Bars, "end_time", cfg.Backtest.EndTime)
	klines, err := rest.GetHistoryKlines(cfg.Strategy.InstID, bar, cfg.Backtest.Bars, endTS)
	if err != nil {
		fatal("拉取 K 线失败: %v", err)
	}
	if len(klines) == 0 {
		fatal("拉到 0 根 K 线，检查 instId / bar 是否正确")
	}
	slog.Info("K 线就绪", "count", len(klines),
		"first", time.UnixMilli(klines[0].Timestamp).Format("2006-01-02 15:04"),
		"last", time.UnixMilli(klines[len(klines)-1].Timestamp).Format("2006-01-02 15:04"))

	// 构造策略
	strat, err := strategy.New(cfg.Strategy.Name)
	if err != nil {
		fatal("strategy: %v", err)
	}
	stratCfg := strategy.Config{
		Name:     cfg.Strategy.Name,
		InstID:   cfg.Strategy.InstID,
		InstType: types.InstType(cfg.Strategy.InstType),
		Leverage: cfg.Strategy.Leverage,
		Bar:      bar,
		Params:   cfg.Strategy.Params,
	}

	// 跑回测
	bt, err := backtest.NewRunner(backtest.Config{
		FeeRate:        cfg.Backtest.FeeRate,
		InitEquity:     cfg.Backtest.InitEquity,
		Leverage:       cfg.Backtest.Leverage,
		StopLossPct:    cfg.Backtest.StopLossPct,
		MaxDrawdownPct: cfg.Backtest.MaxDrawdownPct,
		Verbose:        cfg.Backtest.Verbose,
		TradesCSV:      cfg.Backtest.TradesCSV,
	}, stratCfg, strat)
	if err != nil {
		fatal("init runner: %v", err)
	}

	stats, err := bt.Run(klines)
	if err != nil {
		fatal("run: %v", err)
	}

	// 输出
	fmt.Println()
	fmt.Println(stats.Format())

	if cfg.Backtest.TradesCSV != "" {
		if err := backtest.WriteTradesCSV(cfg.Backtest.TradesCSV, bt.Trades()); err != nil {
			slog.Warn("写 trades CSV 失败", "err", err)
		} else {
			slog.Info("成交明细已写入", "path", cfg.Backtest.TradesCSV)
		}
	}

	if cfg.Backtest.EquityCSV != "" {
		if err := backtest.WriteEquityCurveCSV(cfg.Backtest.EquityCSV, bt.EquityCurve(),
			cfg.Backtest.InitEquity, cfg.Backtest.FeeRate); err != nil {
			slog.Warn("写 equity CSV 失败", "err", err)
		} else {
			slog.Info("权益曲线 CSV 已写入", "path", cfg.Backtest.EquityCSV)
		}
	}

	if cfg.Backtest.EquityPNG != "" {
		// PNG 标题用英文，gonum/plot 默认字体不含中文字形
		title := fmt.Sprintf("%s %s | %s | %d bars", cfg.Strategy.InstID, bar,
			cfg.Strategy.Name, len(klines))
		if err := backtest.PlotEquityCurve(cfg.Backtest.EquityPNG, bt.EquityCurve(),
			cfg.Backtest.InitEquity, cfg.Backtest.FeeRate, title); err != nil {
			slog.Warn("画 equity PNG 失败", "err", err)
		} else {
			slog.Info("权益曲线 PNG 已写入", "path", cfg.Backtest.EquityPNG)
		}
	}
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}

func paramString(p map[string]interface{}, key, def string) string {
	if v, ok := p[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

func fatal(format string, args ...interface{}) {
	slog.Error("fatal", "msg", fmt.Sprintf(format, args...))
	os.Exit(1)
}
