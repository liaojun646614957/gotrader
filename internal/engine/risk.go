package engine

import (
	"fmt"
	"sync"
	"time"

	"github.com/kraus/gotrader/internal/config"
	"github.com/kraus/gotrader/internal/types"
)

// RiskGuard 内嵌风控。它不是一个"模块"，是几条硬规则的实现。
//
// 设计哲学：风控代码越简单越好。复杂的风控本身就是 bug 温床。
// 这里只做四件事：
//  1. 单笔下单金额上限
//  2. 单标的持仓金额上限
//  3. 同标的下单频率
//  4. 全局每分钟下单数
//
// 任何一条不满足，直接拒单。不重试，不"智能调整数量"。手滑下错单不能由风控来"挽救"。
type RiskGuard struct {
	cfg config.RiskConfig

	mu          sync.Mutex
	lastOrderAt map[string]time.Time // instID -> 上次下单时间
	orderTimes  []time.Time          // 全局下单时间窗口（最近 1 分钟）
}

func NewRiskGuard(cfg config.RiskConfig) *RiskGuard {
	return &RiskGuard{
		cfg:         cfg,
		lastOrderAt: make(map[string]time.Time),
	}
}

// Check 在下单前调用。返回 nil 表示通过。
//
// markPrice: 市价单时用最新成交价估算金额；限价单用 signal.Price。
// position: 当前该标的持仓金额（USDT 计），无持仓传 0。
func (r *RiskGuard) Check(sig types.Signal, markPrice, positionValueUSDT float64) error {
	now := time.Now()

	price := sig.Price
	if price == 0 {
		price = markPrice
	}
	orderValue := price * sig.Size

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cfg.MaxOrderValueUSDT > 0 && orderValue > r.cfg.MaxOrderValueUSDT {
		return fmt.Errorf("order value %.2f exceeds limit %.2f",
			orderValue, r.cfg.MaxOrderValueUSDT)
	}

	if r.cfg.MaxPositionValueUSDT > 0 && sig.Side == types.SideBuy {
		// 只对加仓方向（买入）做检查；平仓方向不限制
		if positionValueUSDT+orderValue > r.cfg.MaxPositionValueUSDT {
			return fmt.Errorf("position value %.2f + new %.2f exceeds limit %.2f",
				positionValueUSDT, orderValue, r.cfg.MaxPositionValueUSDT)
		}
	}

	// 最小下单间隔只节流"开仓/加仓"（新增风险）。平仓(reduce-only)是降风险动作，
	// 永远不该被节流——否则"先平后开"翻向时，开仓单会被它前面刚发的平仓单卡住。
	if r.cfg.MinOrderIntervalMS > 0 && !sig.ReduceOnly {
		if last, ok := r.lastOrderAt[sig.InstID]; ok {
			elapsed := now.Sub(last).Milliseconds()
			if elapsed < r.cfg.MinOrderIntervalMS {
				return fmt.Errorf("order interval %dms < min %dms for %s",
					elapsed, r.cfg.MinOrderIntervalMS, sig.InstID)
			}
		}
	}

	if r.cfg.MaxOrdersPerMinute > 0 {
		// 滑动窗口：丢弃 1 分钟前的记录
		cutoff := now.Add(-time.Minute)
		idx := 0
		for ; idx < len(r.orderTimes); idx++ {
			if r.orderTimes[idx].After(cutoff) {
				break
			}
		}
		r.orderTimes = r.orderTimes[idx:]
		if len(r.orderTimes) >= r.cfg.MaxOrdersPerMinute {
			return fmt.Errorf("order rate %d/min exceeds limit %d",
				len(r.orderTimes), r.cfg.MaxOrdersPerMinute)
		}
	}

	// 通过 -> 记账。
	// 频率窗口计入所有订单（交易所限频不区分开/平）。
	r.orderTimes = append(r.orderTimes, now)
	// 最小间隔只用开仓/加仓打点；平仓不打点，避免卡住紧随其后的翻向开仓单。
	if !sig.ReduceOnly {
		r.lastOrderAt[sig.InstID] = now
	}
	return nil
}
