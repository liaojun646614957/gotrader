package okx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/kraus/gotrader/internal/types"
)

// =====================================================================
// 公共行情
// =====================================================================

// rawKline OKX 返回的 K 线是 string 数组：[ts, o, h, l, c, vol, volCcy, volCcyQuote, confirm]
// confirm: "0"=尚未收盘，"1"=已收盘。策略决策只能使用 confirm=1。
type rawKline [9]string

// GetKlines 拉取最近的 K 线。bar 例: "1m" / "5m" / "1H" / "1D"。limit 默认 100，最大 300。
//
// 注意：此接口返回的是 OKX 最新若干根。要拉历史更老的数据用 GetHistoryKlines。
// 返回切片是 OKX 原样：时间倒序（最新在前）。
func (c *Client) GetKlines(instID, bar string, limit int) ([]types.Kline, error) {
	return c.fetchKlines("/api/v5/market/candles", instID, bar, limit, 0)
}

// GetHistoryKlines 分页拉取任意根数的历史 K 线。
//
// total 是想要的总根数（按时间正序返回）。endTS（毫秒）为 0 时从"最新"开始往老拉，
// 否则从 endTS 时刻开始往老拉。这适合"从某天起回测过去 N 根"的场景。
//
// 用 /api/v5/market/history-candles，单次最大 100 根（OKX 限制）。
// 为防止限速（公开接口 20 次/2s），每次请求间隔 120ms。
func (c *Client) GetHistoryKlines(instID, bar string, total int, endTS int64) ([]types.Kline, error) {
	if total <= 0 {
		return nil, nil
	}
	const pageSize = 100
	const sleepBetween = 120 * time.Millisecond

	all := make([]types.Kline, 0, total)
	after := endTS // OKX after = 返回此时间之前的数据
	for len(all) < total {
		want := total - len(all)
		if want > pageSize {
			want = pageSize
		}
		page, err := c.fetchKlines("/api/v5/market/history-candles", instID, bar, want, after)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break // OKX 没数据了
		}
		all = append(all, page...)
		// page 是时间倒序，最末一根是这页里最老的。下一次 after 用它的 ts。
		after = page[len(page)-1].Timestamp
		// 防止打 rate limit
		if len(all) < total {
			time.Sleep(sleepBetween)
		}
	}

	// 反转为时间正序（旧 → 新），便于回测顺序喂入。
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all, nil
}

// fetchKlines 单次拉取，封装两个 endpoint 的公共逻辑。
// after=0 表示不传 after 参数。
func (c *Client) fetchKlines(path, instID, bar string, limit int, after int64) ([]types.Kline, error) {
	q := map[string]string{
		"instId": instID,
		"bar":    bar,
	}
	if limit > 0 {
		q["limit"] = strconv.Itoa(limit)
	}
	if after > 0 {
		q["after"] = strconv.FormatInt(after, 10)
	}
	data, err := c.request("GET", path, q, nil, false)
	if err != nil {
		return nil, err
	}
	var raws []rawKline
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("decode klines: %w", err)
	}
	out := make([]types.Kline, 0, len(raws))
	for _, r := range raws {
		// OKX 的 candles/history-candles 都可能返回当前尚未收盘的 K 线。
		// 盘中 close 持续变化；若拿它做 TSMOM 决策，重复运行 oneshot 会在零轴附近
		// 反复翻多/翻空。这里只保留已确认收盘的数据，调用方无需重复判断。
		if r[8] != "1" {
			continue
		}
		ts, _ := strconv.ParseInt(r[0], 10, 64)
		o, _ := strconv.ParseFloat(r[1], 64)
		h, _ := strconv.ParseFloat(r[2], 64)
		l, _ := strconv.ParseFloat(r[3], 64)
		cl, _ := strconv.ParseFloat(r[4], 64)
		v, _ := strconv.ParseFloat(r[5], 64)
		out = append(out, types.Kline{
			InstID:    instID,
			Timestamp: ts,
			Open:      o,
			High:      h,
			Low:       l,
			Close:     cl,
			Volume:    v,
		})
	}
	return out, nil
}

// =====================================================================
// 账户
// =====================================================================

type rawBalanceResp struct {
	Details []struct {
		Ccy       string `json:"ccy"`
		AvailEq   string `json:"availEq"`
		FrozenBal string `json:"frozenBal"`
		Eq        string `json:"eq"`
	} `json:"details"`
}

// GetBalances 查询交易账户余额。
func (c *Client) GetBalances() ([]types.Balance, error) {
	data, err := c.request("GET", "/api/v5/account/balance", nil, nil, true)
	if err != nil {
		return nil, err
	}
	var arr []rawBalanceResp
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("decode balance: %w", err)
	}
	if len(arr) == 0 {
		return nil, nil
	}
	out := make([]types.Balance, 0, len(arr[0].Details))
	for _, d := range arr[0].Details {
		avail, _ := strconv.ParseFloat(d.AvailEq, 64)
		frozen, _ := strconv.ParseFloat(d.FrozenBal, 64)
		eq, _ := strconv.ParseFloat(d.Eq, 64)
		out = append(out, types.Balance{
			Currency:  d.Ccy,
			Available: avail,
			Frozen:    frozen,
			Equity:    eq,
		})
	}
	return out, nil
}

type rawPosition struct {
	InstID   string `json:"instId"`
	InstType string `json:"instType"`
	PosSide  string `json:"posSide"`
	Pos      string `json:"pos"`
	AvgPx    string `json:"avgPx"`
	Lever    string `json:"lever"`
	Upl      string `json:"upl"`
	MgnMode  string `json:"mgnMode"`
}

// GetPositions 查询持仓。instType 为空时返回全部。
func (c *Client) GetPositions(instType types.InstType) ([]types.Position, error) {
	var q map[string]string
	if instType != "" {
		q = map[string]string{"instType": string(instType)}
	}
	data, err := c.request("GET", "/api/v5/account/positions", q, nil, true)
	if err != nil {
		return nil, err
	}
	var raws []rawPosition
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("decode positions: %w", err)
	}
	out := make([]types.Position, 0, len(raws))
	for _, r := range raws {
		pos, _ := strconv.ParseFloat(r.Pos, 64)
		if pos == 0 {
			continue
		}
		px, _ := strconv.ParseFloat(r.AvgPx, 64)
		lev, _ := strconv.Atoi(r.Lever)
		upl, _ := strconv.ParseFloat(r.Upl, 64)
		out = append(out, types.Position{
			InstID:        r.InstID,
			InstType:      types.InstType(r.InstType),
			PosSide:       types.PosSide(r.PosSide),
			Size:          pos,
			AvgPrice:      px,
			Leverage:      lev,
			UnrealizedPnL: upl,
			MarginMode:    r.MgnMode,
		})
	}
	return out, nil
}

// SetLeverage 设置杠杆。mgnMode: "cross" / "isolated"。posSide 双向持仓时填 long/short，单向填 ""。
func (c *Client) SetLeverage(instID string, leverage int, mgnMode string, posSide types.PosSide) error {
	body := map[string]interface{}{
		"instId":  instID,
		"lever":   strconv.Itoa(leverage),
		"mgnMode": mgnMode,
	}
	if posSide != types.PosNone {
		body["posSide"] = string(posSide)
	}
	_, err := c.request("POST", "/api/v5/account/set-leverage", nil, body, true)
	return err
}

// =====================================================================
// 交易
// =====================================================================

// PlaceOrderReq 下单请求。tdMode: "cash"（现货）/ "cross"（全仓）/ "isolated"（逐仓）。
type PlaceOrderReq struct {
	InstID     string
	TdMode     string
	Side       types.Side
	PosSide    types.PosSide // 双向持仓必填
	Type       types.OrderType
	Price      float64 // 限价单填，市价单为 0
	Size       float64
	ClientOID  string // 自定义订单 ID
	ReduceOnly bool
}

type rawPlaceResp struct {
	OrdID   string `json:"ordId"`
	ClOrdID string `json:"clOrdId"`
	SCode   string `json:"sCode"`
	SMsg    string `json:"sMsg"`
}

// PlaceOrder 下单。返回 OKX 订单 ID。
func (c *Client) PlaceOrder(r PlaceOrderReq) (orderID string, err error) {
	body := map[string]interface{}{
		"instId":  r.InstID,
		"tdMode":  r.TdMode,
		"side":    string(r.Side),
		"ordType": string(r.Type),
		"sz":      formatFloat(r.Size),
	}
	if r.Type != types.OrderMarket {
		body["px"] = formatFloat(r.Price)
	}
	if r.PosSide != types.PosNone {
		body["posSide"] = string(r.PosSide)
	}
	if r.ClientOID != "" {
		body["clOrdId"] = r.ClientOID
	}
	if r.ReduceOnly {
		body["reduceOnly"] = true
	}

	data, err := c.request("POST", "/api/v5/trade/order", nil, body, true)
	if err != nil {
		return "", err
	}
	var arr []rawPlaceResp
	if err := json.Unmarshal(data, &arr); err != nil {
		return "", fmt.Errorf("decode place: %w", err)
	}
	if len(arr) == 0 {
		return "", fmt.Errorf("empty place response")
	}
	if arr[0].SCode != "0" {
		return "", fmt.Errorf("place rejected: sCode=%s sMsg=%s", arr[0].SCode, arr[0].SMsg)
	}
	return arr[0].OrdID, nil
}

// CancelOrder 撤单。orderID 和 clientOID 至少传一个。
func (c *Client) CancelOrder(instID, orderID, clientOID string) error {
	body := map[string]interface{}{"instId": instID}
	if orderID != "" {
		body["ordId"] = orderID
	}
	if clientOID != "" {
		body["clOrdId"] = clientOID
	}
	_, err := c.request("POST", "/api/v5/trade/cancel-order", nil, body, true)
	return err
}

// GetOrder 查询单个订单。
func (c *Client) GetOrder(instID, orderID, clientOID string) (*types.Order, error) {
	q := map[string]string{"instId": instID}
	if orderID != "" {
		q["ordId"] = orderID
	}
	if clientOID != "" {
		q["clOrdId"] = clientOID
	}
	data, err := c.request("GET", "/api/v5/trade/order", q, nil, true)
	if err != nil {
		return nil, err
	}
	var raws []rawOrder
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("decode order: %w", err)
	}
	if len(raws) == 0 {
		return nil, nil
	}
	return raws[0].toOrder(), nil
}

type rawOrder struct {
	InstID   string `json:"instId"`
	InstType string `json:"instType"`
	OrdID    string `json:"ordId"`
	ClOrdID  string `json:"clOrdId"`
	Side     string `json:"side"`
	PosSide  string `json:"posSide"`
	OrdType  string `json:"ordType"`
	Px       string `json:"px"`
	Sz       string `json:"sz"`
	State    string `json:"state"`
	FillSz   string `json:"fillSz"`
	AvgPx    string `json:"avgPx"`
	Lever    string `json:"lever"`
	CTime    string `json:"cTime"`
	UTime    string `json:"uTime"`
}

func (r *rawOrder) toOrder() *types.Order {
	px, _ := strconv.ParseFloat(r.Px, 64)
	sz, _ := strconv.ParseFloat(r.Sz, 64)
	fsz, _ := strconv.ParseFloat(r.FillSz, 64)
	avg, _ := strconv.ParseFloat(r.AvgPx, 64)
	lev, _ := strconv.Atoi(r.Lever)
	ctMs, _ := strconv.ParseInt(r.CTime, 10, 64)
	utMs, _ := strconv.ParseInt(r.UTime, 10, 64)
	return &types.Order{
		ClientOrderID: r.ClOrdID,
		OrderID:       r.OrdID,
		InstID:        r.InstID,
		InstType:      types.InstType(r.InstType),
		Side:          types.Side(r.Side),
		PosSide:       types.PosSide(r.PosSide),
		Type:          types.OrderType(r.OrdType),
		Price:         px,
		Size:          sz,
		Leverage:      lev,
		Status:        mapOrderStatus(r.State),
		FilledSize:    fsz,
		AvgPrice:      avg,
		CreatedAt:     time.UnixMilli(ctMs),
		UpdatedAt:     time.UnixMilli(utMs),
	}
}

func mapOrderStatus(state string) types.OrderStatus {
	switch state {
	case "live":
		return types.StatusLive
	case "partially_filled":
		return types.StatusPartiallyFilled
	case "filled":
		return types.StatusFilled
	case "canceled":
		return types.StatusCanceled
	default:
		// OKX 还有 mmp_canceled 等，归到 canceled
		return types.StatusCanceled
	}
}

// formatFloat 用 OKX 接受的格式（不要科学计数法，最多 8 位小数）。
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
