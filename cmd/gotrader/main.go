// gotrader: OKX 量化交易主程序。
//
// 启动顺序：加载配置 → 构造 REST/WS 客户端 → 同步账户/持仓 →
// 启动 WS → 跑策略主循环 → 等待信号 → SIGINT 优雅退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kraus/gotrader/internal/config"
	"github.com/kraus/gotrader/internal/engine"
	"github.com/kraus/gotrader/internal/exchange/okx"
	"github.com/kraus/gotrader/internal/notify"
	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}

	setupLogger(cfg.LogLevel)

	// 实盘前的安全护栏：必须显式 live: true
	if cfg.OKX.Live {
		slog.Warn("=== 实盘模式：真金白银，谨慎操作 ===")
	} else {
		slog.Info("运行于 OKX 模拟盘")
	}

	rest := okx.NewClient(cfg.OKX.APIKey, cfg.OKX.SecretKey, cfg.OKX.Passphrase,
		cfg.OKX.RestURL, !cfg.OKX.Live)

	wsPub := okx.NewWSClient(cfg.OKX.WSPublic, false, "", "", "")
	wsBiz := okx.NewWSClient(cfg.OKX.WSBusiness, false, "", "", "")
	wsPriv := okx.NewWSClient(cfg.OKX.WSPrivate, true,
		cfg.OKX.APIKey, cfg.OKX.SecretKey, cfg.OKX.Passphrase)

	risk := engine.NewRiskGuard(cfg.Risk)

	strat, err := strategy.New(cfg.Strategy.Name)
	if err != nil {
		fatal("strategy: %v", err)
	}

	stratCfg := strategy.Config{
		Name:     cfg.Strategy.Name,
		InstID:   cfg.Strategy.InstID,
		InstType: types.InstType(cfg.Strategy.InstType),
		Leverage: cfg.Strategy.Leverage,
		Bar:      paramString(cfg.Strategy.Params, "bar", "1m"),
		Params:   cfg.Strategy.Params,
	}

	// 合约：先设杠杆。错就直接退出，别假装没看见。
	if stratCfg.InstType == types.InstSwap || stratCfg.InstType == types.InstFutures {
		if stratCfg.Leverage <= 0 {
			fatal("strategy.leverage 必须 > 0（合约交易）")
		}
		if err := rest.SetLeverage(stratCfg.InstID, stratCfg.Leverage, "cross", types.PosNone); err != nil {
			slog.Warn("设置杠杆失败（可能已设置过）", "err", err)
		}
	}

	notifier := notify.New(cfg.Notify)

	eng := engine.New(rest, wsPub, wsBiz, wsPriv, risk, strat, stratCfg, notifier)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := eng.Run(ctx); err != nil {
		fatal("engine: %v", err)
	}
	slog.Info("已退出")
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
