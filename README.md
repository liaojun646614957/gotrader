# gotrader

OKX 量化交易程序，Go 实现。支持现货（SPOT）和永续合约（SWAP）。

> 个人自用 MVP。不是通用框架，不做多交易所抽象。

## 设计原则

1. **数据结构优先**：现货和合约共用一套 `Order`/`Position`/`Balance`，靠 `InstType` 字段区分，消除调用方的 if/else。
2. **风控内嵌**：风控不是模块，是 `engine.PlaceOrder` 里强制执行的拦截器（最大单笔金额、最大持仓、下单频率）。
3. **Testnet 默认**：实盘必须显式 `live: true`，避免手滑爆仓。
4. **事件驱动**：行情通过 channel 推到策略，策略产生信号，engine 统一下单。无锁主循环。
5. **WebSocket 一等公民**：行情和订单/持仓推送都走 WS，REST 只用于下单/撤单/历史拉取。

## 目录结构

```
cmd/gotrader/         入口
internal/types/       核心数据结构
internal/exchange/okx REST + WebSocket 客户端
internal/engine/      主循环 + 风控
internal/strategy/    策略接口 + 示例
internal/config/      YAML 配置
```

## 快速开始

```bash
# 1. 编译
go build -o bin/gotrader ./cmd/gotrader

# 2. 准备配置（复制模板，填入你的 API Key）
cp config.example.yaml config.yaml
# 编辑 config.yaml

# 3. 在 OKX 模拟盘跑（live: false）
./bin/gotrader -config config.yaml
```

## 风控参数（必填）

`config.yaml` 里的 `risk` 段是硬约束，违反直接拒单：

- `max_order_value_usdt`：单笔最大下单金额（U 本位）
- `max_position_value_usdt`：标的最大持仓金额
- `min_order_interval_ms`：同一标的两次下单最小间隔
- `max_orders_per_minute`：全局下单频率

## 安全

- API Key 写在 `config.yaml`（已 gitignore），**绝对不要提交**
- 实盘前在模拟盘至少跑 7 天
- 设置 IP 白名单，关闭 OKX API 的提币权限

## 路线图（按需添加，不预设）

- [ ] 数据持久化（PostgreSQL 存订单/成交）
- [ ] 回测引擎
- [ ] Web 控制台
- [ ] 多账户

不会做的事：
- ❌ 多交易所抽象（真要加币安，到时再抽）
- ❌ 插件化策略加载（一个 import 比 plugin 简单 100 倍）
