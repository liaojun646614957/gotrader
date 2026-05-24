// Package strategy 定义策略接口和注册表。
//
// 接口故意做得很小：行情来了通知策略，策略往 signals chan 写信号。
// 不搞继承、不搞生命周期 hooks 一大堆。需要状态自己存。
package strategy

import (
	"fmt"

	"github.com/kraus/gotrader/internal/types"
)

// Config 策略实例化配置。Engine 从 yaml 里读出来交给策略。
type Config struct {
	Name     string
	InstID   string
	InstType types.InstType
	Leverage int
	Bar      string                 // K 线周期: "1m" / "5m" / "1H"
	Params   map[string]interface{} // 策略私有参数
}

// Strategy 策略接口。每个回调里不要做阻塞操作，信号要异步走 signals channel。
type Strategy interface {
	Init(cfg Config, signals chan<- types.Signal) error
	// WarmupBars 返回启动时需要预热的历史 K 线数量。0 表示不需要预热。
	// 在 Init 之后调用，所以策略可以根据已解析的参数返回。
	WarmupBars() int
	OnTick(t types.Tick)
	OnKline(k types.Kline)
	OnOrderUpdate(o types.Order)
}

// ForceEvaluator 可选接口。实现了这个接口的策略在 oneshot 模式下，
// engine 会用 ForceEvaluate 取代 OnKline 喂"决策 K"，保证无论
// 策略内部的频控计数器（如 TSMOM 的 barsSinceEval）处于什么状态，
// 决策 K 必然触发一次完整评估并产出信号。
//
// 设计取舍：
//   - 没实现这个接口的策略（比如 chan_bs）走原路径，零破坏。
//   - 实现的策略可以把"预热期间的频控计数"清零，相当于告诉策略：
//     "这是 oneshot 触发，cron 频率已经替你做了 holding_bars 的限频"。
type ForceEvaluator interface {
	// ForceEvaluate 把决策 K 喂给策略并强制评估，无视任何内部频控。
	// 实现方需要保证：调用后如果数据充足，必然产出（或拒绝产出）一个决策。
	ForceEvaluate(k types.Kline)
}

// Factory 创建策略实例。
type Factory func() Strategy

var registry = map[string]Factory{}

// Register 在 init() 里调用。
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("strategy already registered: " + name)
	}
	registry[name] = f
}

// New 按名字创建策略实例。
func New(name string) (Strategy, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown strategy: %s", name)
	}
	return f(), nil
}

// paramFloat 安全读 float 参数。yaml 解出来可能是 int / float64。
func paramFloat(p map[string]interface{}, key string, def float64) float64 {
	v, ok := p[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return def
}

func paramInt(p map[string]interface{}, key string, def int) int {
	return int(paramFloat(p, key, float64(def)))
}
