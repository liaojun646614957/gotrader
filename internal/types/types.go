// Package types 定义跨模块共享的核心数据结构。
//
// 设计原则：现货和合约共用同一套结构，用 InstType 字段区分，
// 调用方不需要写 if SPOT else SWAP 这种垃圾代码。
package types

import (
	"fmt"
	"time"
)

// InstType 标的类型。OKX 把现货和合约分得很清楚，我们沿用它的命名。
type InstType string

const (
	InstSpot    InstType = "SPOT"    // 现货
	InstSwap    InstType = "SWAP"    // 永续合约（U 本位 / 币本位由 InstID 后缀决定）
	InstFutures InstType = "FUTURES" // 交割合约（暂不在 MVP 范围）
	InstMargin  InstType = "MARGIN"  // 杠杆现货（暂不在 MVP 范围）
)

// Side 买卖方向。
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// PosSide 合约持仓方向。现货下单时填 ""。
// OKX 单向持仓模式下也填 ""，双向持仓模式必须填 long 或 short。
type PosSide string

const (
	PosNone  PosSide = ""      // 现货 / 单向持仓
	PosLong  PosSide = "long"  // 双向持仓 - 多头
	PosShort PosSide = "short" // 双向持仓 - 空头
)

// OrderType 委托类型。只列我们会用的，其他 OKX 类型（iceberg/twap）暂不支持。
type OrderType string

const (
	OrderMarket    OrderType = "market"     // 市价单
	OrderLimit     OrderType = "limit"      // 限价单
	OrderPostOnly  OrderType = "post_only"  // 只做 maker
	OrderFOK       OrderType = "fok"        // 全部成交或立即取消
	OrderIOC       OrderType = "ioc"        // 立即成交并取消剩余
)

// OrderStatus 订单状态。
type OrderStatus string

const (
	StatusLive            OrderStatus = "live"
	StatusPartiallyFilled OrderStatus = "partially_filled"
	StatusFilled          OrderStatus = "filled"
	StatusCanceled        OrderStatus = "canceled"
	StatusRejected        OrderStatus = "rejected"
)

// Instrument 描述一个可交易标的。
//
// InstID 是 OKX 唯一标识符，例如：
//
//	现货:  "BTC-USDT"
//	永续:  "BTC-USDT-SWAP"
//	交割:  "BTC-USDT-240329"
type Instrument struct {
	InstID   string   // OKX 标的 ID
	InstType InstType // 标的类型
	Base     string   // 基础币，如 BTC
	Quote    string   // 计价币，如 USDT
	TickSize float64  // 价格精度
	LotSize  float64  // 数量精度
}

// Kline K 线。时间戳用毫秒，和 OKX 一致。
type Kline struct {
	InstID    string
	Timestamp int64 // 毫秒
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64 // 成交量（基础币）
}

// Tick 最新成交。
type Tick struct {
	InstID    string
	Timestamp int64
	Price     float64
	Size      float64
	Side      Side
}

// OrderBook 深度快照。Bids/Asks 按价格降/升序排列。
type OrderBook struct {
	InstID    string
	Timestamp int64
	Bids      []PriceLevel
	Asks      []PriceLevel
}

// PriceLevel 一档价位。
type PriceLevel struct {
	Price float64
	Size  float64
}

// Order 订单。
//
// 现货和合约共用：
//   - 现货: PosSide = "", Leverage 忽略
//   - 合约: PosSide 按持仓模式填，Leverage 必填
type Order struct {
	ClientOrderID string  // 我们自己生成的 ID（OKX 字段名 clOrdId）
	OrderID       string  // OKX 返回的 ordId
	InstID        string
	InstType      InstType
	Side          Side
	PosSide       PosSide
	Type          OrderType
	Price         float64 // 限价单必填，市价单为 0
	Size          float64 // 数量。现货/合约语义不同（合约张数）
	Leverage      int     // 合约杠杆，现货为 0
	ReduceOnly    bool    // 仅减仓（合约）。策略可据此区分"开仓"vs"平仓"成交
	Reason        string  // 该订单的备注（如 "stop_loss" / 策略 reason），便于策略状态机分支
	Status        OrderStatus
	FilledSize    float64
	AvgPrice      float64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Position 持仓。
type Position struct {
	InstID       string
	InstType     InstType
	PosSide      PosSide
	Size         float64 // 持仓数量，正数。空仓为 0。
	AvgPrice     float64 // 开仓均价
	Leverage     int
	UnrealizedPnL float64
	MarginMode   string // "cross" / "isolated"
}

// Balance 余额。Currency 是币种代码（如 USDT、BTC）。
type Balance struct {
	Currency  string
	Available float64 // 可用
	Frozen    float64 // 冻结
	Equity    float64 // 权益（合约账户）
}

// Signal 策略发出的交易信号。Engine 收到后跑风控再下单。
type Signal struct {
	InstID     string
	InstType   InstType
	Side       Side
	PosSide    PosSide
	Type       OrderType
	Price      float64 // 限价单填，市价单 0
	Size       float64
	Leverage   int    // 合约用
	ReduceOnly bool   // true=只减仓，不允许开新仓（合约平仓用）
	Reason     string // 信号产生的原因，便于排查
}

// String 便于日志打印。
func (o *Order) String() string {
	return fmt.Sprintf("Order{%s %s %s %s %s price=%v size=%v status=%s}",
		o.InstID, o.InstType, o.Side, o.PosSide, o.Type, o.Price, o.Size, o.Status)
}
