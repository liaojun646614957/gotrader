package engine

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kraus/gotrader/internal/strategy"
	"github.com/kraus/gotrader/internal/types"
)

// RunOnce 一次性评估模式：拉历史 K 线 → 喂策略 → 处理产生的信号 → 退出。
//
// 适用场景：低频策略（1D + holding_bars=5 这种）通过外部 cron 触发，
// 避免常驻进程长时间 idle。
//
// 与 Run 的关键差异：
//   - 不启动 WS（不需要长连接）
//   - 用 GetHistoryKlines 而非 GetKlines，避免拉到当前正在形成的 K 线（防偷看未来）
//   - 喂完 K 线后同步处理 signals chan 里的信号，处理完立即返回
//
// ⚠️ 调用方需要注意：
//   - tsmom 的 barsSinceEval 在每次进程启动时归零。
//     因此 cron 触发频率应当 = bar 周期 × holding_bars（例如 1D × 5 = 每 5 天）。
//     若 cron 频率更高，相当于策略 holding_bars 被强制改成 1，与回测参数不符。
//   - 订单状态回报靠下次启动 bootstrap 时调 GetPositions 拉取，
//     所以 oneshot 适合"市价单 + 立即成交"场景；带挂单/限价单要慎用。
func (e *Engine) RunOnce(ctx context.Context) error {
	_ = ctx // bootstrap 目前没用，保留以便未来扩展

	if err := e.bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	signals := make(chan types.Signal, 64)
	if err := e.strat.Init(e.stratCfg, signals); err != nil {
		return fmt.Errorf("strategy init: %w", err)
	}

	n := e.strat.WarmupBars()
	if n <= 0 {
		n = 1 // 至少 1 根才能驱动 OnKline
	}
	// 多拉 1 根：前 n 根作为预热（信号丢弃），最后 1 根是"今天的决策 K"（信号保留）。
	// GetHistoryKlines 只返回已收盘的 K 线，避免吃到当前正在形成的那根。
	wantBars := n + 1
	klines, err := e.rest.GetHistoryKlines(e.stratCfg.InstID, e.stratCfg.Bar, wantBars, 0)
	if err != nil {
		return fmt.Errorf("get history klines: %w", err)
	}
	if len(klines) == 0 {
		return fmt.Errorf("OKX 返回 0 根 K 线，检查 instId=%s bar=%s",
			e.stratCfg.InstID, e.stratCfg.Bar)
	}
	if len(klines) < 2 {
		// 至少要有 1 根预热 + 1 根决策，否则策略 closes 缓存不够
		return fmt.Errorf("K 线数据不足 (got %d, want %d)", len(klines), wantBars)
	}

	// GetHistoryKlines 已经反转为正序（旧→新）。
	// 前 n 根丢弃信号（预热），最后 1 根保留信号（决策）。
	warmCount := len(klines) - 1
	for i := 0; i < warmCount; i++ {
		e.strat.OnKline(klines[i])
	}
	dropped := drainSignals(signals)

	// ⚠️ 关键步骤：把策略内部"持仓状态"对齐到交易所真实持仓。
	//
	// 为什么需要：预热 K 线让策略自己"推导"了一个 state（比如 SHORT），
	// 但 oneshot 没有 WS 订单回报来矫正这个虚假 state。如果真实账户其实是空仓，
	// 决策 K 评估时策略会"以为自己已经在 SHORT 方向"而拒绝发信号 → cron 白跑。
	//
	// Run 模式不需要这一步，因为长连接 WS 会持续推送订单回报矫正策略 state。
	longQty, shortQty, err := e.syncStrategyToActualPosition()
	if err != nil {
		slog.Warn("同步真实持仓到策略失败，决策可能不准确", "err", err)
	}

	// 喂最后 1 根 = 决策 K。
	// 如果策略实现了 ForceEvaluator，用它绕过策略内部的频控（如 TSMOM 的 holding_bars 计数），
	// 保证 oneshot 触发时一定产出决策——频率由 cron / 手动调用频率承载，不依赖策略巧合。
	// 没实现 ForceEvaluator 的策略走原 OnKline 路径，向后兼容。
	last := klines[warmCount]
	mode := "OnKline"
	if fe, ok := e.strat.(strategy.ForceEvaluator); ok {
		fe.ForceEvaluate(last)
		mode = "ForceEvaluate"
	} else {
		e.strat.OnKline(last)
	}
	slog.Info("oneshot 评估完成",
		"instId", e.stratCfg.InstID,
		"warmup_bars", warmCount,
		"decision_bar_ts", last.Timestamp,
		"decision_close", last.Close,
		"decision_mode", mode,
		"warmup_signals_dropped", dropped,
	)

	// 同步处理决策 K 产生的信号。handleSignal 内部已经做了风控 + 下单 + 邮件通知。
	// 区分"信号产生数 / 成功下单数 / 失败原因"，决策报告才能给出准确结论。
	var signalsEmitted, ordersOK, ordersFail int
	var lastFailErr error
	drain := false
	for !drain {
		select {
		case sig := <-signals:
			signalsEmitted++
			if err := e.handleSignal(sig); err != nil {
				ordersFail++
				lastFailErr = err
			} else {
				ordersOK++
			}
		default:
			drain = true
		}
	}
	slog.Info("oneshot 退出",
		"signals_emitted", signalsEmitted,
		"orders_ok", ordersOK,
		"orders_fail", ordersFail,
	)

	// 打印一段人话总结，让运行者一眼看懂"现在该做什么 / 已经做了什么"。
	// 这一段不用 slog 是因为 slog 的 key=value 格式不利于阅读，
	// 我们要的是"决策报告"的人类语义。
	e.printDecisionSummary(last, longQty, shortQty,
		signalsEmitted, ordersOK, ordersFail, lastFailErr)
	return nil
}

// printDecisionSummary 用人类可读格式输出最终结论。
// last  ：决策 K（已收盘的最近一根），用其 close 估值持仓。
// signalsEmitted / ordersOK / ordersFail ：策略发了几个信号、几个 OKX 接单成功、几个失败。
// lastFailErr   ：最后一次失败的错误（OKX 返回的真实原因）。
func (e *Engine) printDecisionSummary(last types.Kline, longQty, shortQty float64,
	signalsEmitted, ordersOK, ordersFail int, lastFailErr error) {
	contractsToETH, hasCtVal := swapCtVal[e.stratCfg.InstID]
	if !hasCtVal {
		contractsToETH = 1 // 现货或未知合约面值时退化为 1:1
	}

	posSide := "FLAT (空仓)"
	posContracts := 0.0
	if longQty > 0 {
		posSide = "LONG (多)"
		posContracts = longQty
	} else if shortQty > 0 {
		posSide = "SHORT (空)"
		posContracts = shortQty
	}
	posBaseQty := posContracts * contractsToETH
	posNotional := posBaseQty * last.Close

	// 决策结论分四种基础场景，但下单失败有独立的优先级（最严重）。
	var conclusion, advice string
	switch {
	case ordersFail > 0 && ordersOK == 0:
		conclusion = fmt.Sprintf("🚨 失败：策略产生 %d 个信号，OKX 全部拒绝下单", signalsEmitted)
		advice = fmt.Sprintf("OKX 拒单原因：%v\n  → 检查账户余额、保证金、持仓模式、风控配置后再跑。", lastFailErr)
	case ordersFail > 0:
		conclusion = fmt.Sprintf("⚠️ 部分失败：成功 %d 笔，失败 %d 笔（共 %d 个信号）",
			ordersOK, ordersFail, signalsEmitted)
		advice = fmt.Sprintf("最近一次失败原因：%v\n  → 去 OKX 后台核对哪些单成了，再决定下一步。", lastFailErr)
	case ordersOK == 0 && longQty == 0 && shortQty == 0:
		conclusion = "✋ 无操作：策略目标=空仓，账户也空仓"
		advice = "什么都不用做。下次触发会重新评估。"
	case ordersOK == 0:
		conclusion = "✋ 无操作：策略目标方向 = 当前持仓方向，无需调仓"
		advice = "继续持有当前仓位。下次触发若方向有变才会下单。"
	case ordersOK == 1:
		conclusion = "📤 已成功下 1 笔订单"
		advice = "去 OKX 后台查订单详情确认成交价。再跑一次 oneshot 应该看到\"无操作\"。"
	case ordersOK >= 2:
		conclusion = fmt.Sprintf("🔄 已成功下 %d 笔订单（典型场景：先平后反向开 = 换向）", ordersOK)
		advice = "去 OKX 后台确认两笔订单都成交。再跑一次 oneshot 应该看到\"无操作\"。"
	}

	fmt.Println()
	fmt.Println("=============== oneshot 决策报告 ===============")
	fmt.Printf("  标的         : %s (%s)\n", e.stratCfg.InstID, e.stratCfg.InstType)
	fmt.Printf("  评估时点     : %s (K线收盘价 %.2f USDT)\n",
		time.UnixMilli(last.Timestamp).Format("2006-01-02 15:04 MST"), last.Close)
	fmt.Println("")
	fmt.Println("  --- 当前账户持仓 ---")
	fmt.Printf("  方向         : %s\n", posSide)
	if posContracts > 0 {
		fmt.Printf("  张数         : %.0f 张 (= %.4f %s)\n", posContracts, posBaseQty,
			baseCcyOf(e.stratCfg.InstID))
		fmt.Printf("  名义价值     : %.2f USDT\n", posNotional)
	}
	fmt.Println("")
	fmt.Printf("  --- 本次执行：信号 %d / 成功 %d / 失败 %d ---\n",
		signalsEmitted, ordersOK, ordersFail)
	fmt.Println("")
	fmt.Println("  --- 决策结论 ---")
	fmt.Printf("  %s\n", conclusion)
	fmt.Println("")
	fmt.Println("  --- 建议 ---")
	fmt.Printf("  %s\n", advice)
	fmt.Println("=================================================")
	fmt.Println()
}

// baseCcyOf 从 OKX instId 里抠出基础币，例如 "ETH-USDT-SWAP" → "ETH"。
func baseCcyOf(instID string) string {
	for i := 0; i < len(instID); i++ {
		if instID[i] == '-' {
			return instID[:i]
		}
	}
	return instID
}

// syncStrategyToActualPosition 把策略的内部持仓 state 对齐到交易所真实持仓。
// 返回 longQty, shortQty（OKX 张数）供 oneshot 决策汇总打印。
//
// 两种情形：
//   - 真实持仓 = 0    → 注入 ReduceOnly+Filled 事件，触发策略把 state 重置为 FLAT
//                       （tsmom / chan_bs 都在 OnOrderUpdate 里实现了这个逻辑）
//   - 真实持仓 != 0   → 若策略实现了 PositionSyncer，用真实方向 + 数量强制覆盖其 state；
//                       否则退回"沿用预热推导值"（可能与真实持仓方向相反，见下）。
//
// 为什么真实持仓非零也必须对齐：预热推导的方向可能和账户真实持仓相反
// （如推导 SHORT、实际 LONG），策略会以为要"平掉 SHORT"而发同方向 reduce-only，
// 被 OKX 51170 拒单。SyncPosition 把 state/curQty 钉到真相上，从根上消除这个问题。
func (e *Engine) syncStrategyToActualPosition() (longQty, shortQty float64, err error) {
	positions, err := e.rest.GetPositions("")
	if err != nil {
		return 0, 0, err
	}
	for _, p := range positions {
		if p.InstID != e.stratCfg.InstID {
			continue
		}
		switch p.PosSide {
		case types.PosLong:
			// 双向持仓 long_short 模式：明确多头
			longQty += p.Size
		case types.PosShort:
			// 双向持仓 long_short 模式：明确空头
			shortQty += p.Size
		default:
			// net 模式（PosSide="net"）或其他未知值：按 size 正负判断。
			// OKX net 模式下 pos 字段是带符号的：正数=多头，负数=空头。
			if p.Size > 0 {
				longQty += p.Size
			} else if p.Size < 0 {
				shortQty += -p.Size
			}
		}
	}

	if longQty == 0 && shortQty == 0 {
		// 真实空仓 → 强制策略 state = FLAT
		slog.Info("交易所真实持仓为空，强制策略 state=FLAT", "instId", e.stratCfg.InstID)
		e.strat.OnOrderUpdate(types.Order{
			InstID:     e.stratCfg.InstID,
			InstType:   e.stratCfg.InstType,
			Status:     types.StatusFilled,
			ReduceOnly: true,
			Reason:     "oneshot_sync_flat",
		})
		return longQty, shortQty, nil
	}

	// 真实持仓非零：把策略 state 对齐到真实方向 + 数量，纠正预热推导的错误方向。
	dir := types.PosLong
	contracts := longQty
	if shortQty > 0 {
		dir, contracts = types.PosShort, shortQty
	}
	if syncer, ok := e.strat.(strategy.PositionSyncer); ok {
		baseQty := contractsToBaseQty(e.stratCfg.InstID, contracts)
		syncer.SyncPosition(dir, baseQty)
		slog.Info("已把策略 state 对齐到交易所真实持仓",
			"instId", e.stratCfg.InstID,
			"dir", dir,
			"base_qty", baseQty,
			"long_qty", longQty,
			"short_qty", shortQty,
		)
	} else {
		slog.Info("策略未实现 PositionSyncer，state 沿用预热推导值（可能与真实持仓不一致）",
			"instId", e.stratCfg.InstID,
			"long_qty", longQty,
			"short_qty", shortQty,
		)
	}
	return longQty, shortQty, nil
}
