// Package config 加载并校验 YAML 配置。
package config

import (
	"fmt"
	"os"

	"github.com/kraus/gotrader/internal/notify"
	"gopkg.in/yaml.v3"
)

// Config 应用根配置。
type Config struct {
	OKX      OKXConfig          `yaml:"okx"`
	Risk     RiskConfig         `yaml:"risk"`
	Strategy StrategyConfig     `yaml:"strategy"`
	Backtest BacktestConfig     `yaml:"backtest"`
	Notify   notify.SMTPConfig  `yaml:"notify"`
	LogLevel string             `yaml:"log_level"`
}

// BacktestConfig 回测参数。仅 cmd/backtest 用，实盘程序忽略。
type BacktestConfig struct {
	Bars       int     `yaml:"bars"`        // 拉取 K 线根数，默认 1000
	FeeRate    float64 `yaml:"fee_rate"`    // 单边手续费率，默认 0.001（0.1%）
	InitEquity float64 `yaml:"init_equity"` // 初始资金（USDT），默认 10000
	// Leverage 与 strategy.Leverage 配合：策略发出 Size=0 的信号时，
	// runner 自动按 equity * leverage / price 计算实际仓位（"全仓"模式）。
	// 默认 0 = 关闭自动全仓，按 strategy.params.size 字面值下单。
	Leverage float64 `yaml:"leverage"`
	// StopLossPct 单笔止损比例（按"价格反向波动"算）。
	// 多头：价格跌 N% 触发；空头：价格涨 N% 触发。0 = 禁用。
	StopLossPct float64 `yaml:"stop_loss_pct"`
	// MaxDrawdownPct 账户最大回撤上限（相对历史峰值）。
	// 触发后停止开新仓（已有仓位走自然平仓 / 止损）。0 = 禁用。
	MaxDrawdownPct float64 `yaml:"max_drawdown_pct"`
	Verbose        bool    `yaml:"verbose"` // 打印每笔成交日志
	TradesCSV  string  `yaml:"trades_csv"`  // 非空则把成交明细写到该文件
	EquityCSV  string  `yaml:"equity_csv"`  // 非空则把权益曲线写到该 CSV
	EquityPNG  string  `yaml:"equity_png"`  // 非空则画权益曲线 PNG
	EndTime    string  `yaml:"end_time"`    // 可选，回测截止日期（格式 2006-01-02），空=至今
}

// OKXConfig 交易所连接配置。Live=false 时强制使用模拟盘，无论 base_url 写啥。
type OKXConfig struct {
	APIKey     string `yaml:"api_key"`
	SecretKey  string `yaml:"secret_key"`
	Passphrase string `yaml:"passphrase"`
	Live       bool   `yaml:"live"`     // false=模拟盘（默认）, true=实盘
	RestURL    string `yaml:"rest_url"` // 可选，覆盖默认
	WSPublic   string `yaml:"ws_public"`
	WSPrivate  string `yaml:"ws_private"`
	WSBusiness string `yaml:"ws_business"` // K 线等
}

// RiskConfig 风控硬约束。任何字段为 0 视为未启用该项检查。
type RiskConfig struct {
	MaxOrderValueUSDT    float64 `yaml:"max_order_value_usdt"`    // 单笔下单最大金额（按报价币计算）
	MaxPositionValueUSDT float64 `yaml:"max_position_value_usdt"` // 单标的持仓最大金额
	MinOrderIntervalMS   int64   `yaml:"min_order_interval_ms"`   // 同标的两次下单最小间隔
	MaxOrdersPerMinute   int     `yaml:"max_orders_per_minute"`   // 全局每分钟最大下单数
}

// StrategyConfig 策略参数。
// MVP 阶段只跑一个策略实例。要跑多个再扩展为 []。
type StrategyConfig struct {
	Name      string                 `yaml:"name"`      // 策略名，与代码里注册的对上
	InstID    string                 `yaml:"inst_id"`
	InstType  string                 `yaml:"inst_type"` // SPOT 或 SWAP
	Leverage  int                    `yaml:"leverage"`  // 合约杠杆
	Params    map[string]interface{} `yaml:"params"`    // 策略私有参数
}

// Load 读 YAML 并做实盘/模拟盘所需的完整校验（含 API 凭证 + 风控）。
func Load(path string) (*Config, error) {
	c, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	c.applyDefaults()
	return c, nil
}

// LoadForBacktest 加载配置，但不要求 API 凭证（OKX 公开行情接口不需要签名）。
// 也不要求风控配置，因为回测不下真实订单。
func LoadForBacktest(path string) (*Config, error) {
	c, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	if c.Strategy.Name == "" {
		return nil, fmt.Errorf("strategy.name 必填")
	}
	if c.Strategy.InstID == "" {
		return nil, fmt.Errorf("strategy.inst_id 必填")
	}
	c.applyDefaults()
	c.applyBacktestDefaults()
	return c, nil
}

func parseFile(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.OKX.APIKey == "" || c.OKX.SecretKey == "" || c.OKX.Passphrase == "" {
		return fmt.Errorf("okx.api_key / secret_key / passphrase 必填")
	}
	if c.Strategy.Name == "" {
		return fmt.Errorf("strategy.name 必填")
	}
	if c.Strategy.InstID == "" {
		return fmt.Errorf("strategy.inst_id 必填")
	}
	if c.Risk.MaxOrderValueUSDT <= 0 {
		return fmt.Errorf("risk.max_order_value_usdt 必填且 >0（防手滑）")
	}
	return nil
}

func (c *Config) applyBacktestDefaults() {
	if c.Backtest.Bars <= 0 {
		c.Backtest.Bars = 1000
	}
	if c.Backtest.FeeRate <= 0 {
		c.Backtest.FeeRate = 0.001
	}
	if c.Backtest.InitEquity <= 0 {
		c.Backtest.InitEquity = 10_000
	}
}

func (c *Config) applyDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	// REST endpoint
	if c.OKX.RestURL == "" {
		if c.OKX.Live {
			c.OKX.RestURL = "https://www.okx.com"
		} else {
			c.OKX.RestURL = "https://www.okx.com" // 模拟盘共用域名，靠 header x-simulated-trading 区分
		}
	}
	if c.OKX.WSPublic == "" {
		if c.OKX.Live {
			c.OKX.WSPublic = "wss://ws.okx.com:8443/ws/v5/public"
		} else {
			c.OKX.WSPublic = "wss://wspap.okx.com:8443/ws/v5/public"
		}
	}
	if c.OKX.WSPrivate == "" {
		if c.OKX.Live {
			c.OKX.WSPrivate = "wss://ws.okx.com:8443/ws/v5/private"
		} else {
			c.OKX.WSPrivate = "wss://wspap.okx.com:8443/ws/v5/private"
		}
	}
	if c.OKX.WSBusiness == "" {
		if c.OKX.Live {
			c.OKX.WSBusiness = "wss://ws.okx.com:8443/ws/v5/business"
		} else {
			c.OKX.WSBusiness = "wss://wspap.okx.com:8443/ws/v5/business"
		}
	}
}
