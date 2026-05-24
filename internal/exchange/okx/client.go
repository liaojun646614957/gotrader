// Package okx 实现 OKX V5 API 客户端（REST + WebSocket）。
//
// 我们只对接 OKX，不做"多交易所抽象"。等真要加币安再抽，
// 提前抽象只会得到一个适配所有交易所的怪物 interface（YAGNI）。
package okx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client OKX REST 客户端。线程安全（http.Client 本身就线程安全）。
type Client struct {
	apiKey     string
	secretKey  string
	passphrase string
	restURL    string
	simulated  bool // 模拟盘开关，控制 x-simulated-trading header
	http       *http.Client
}

// NewClient 构造 REST 客户端。simulated=true 时所有请求带 x-simulated-trading: 1。
func NewClient(apiKey, secretKey, passphrase, restURL string, simulated bool) *Client {
	return &Client{
		apiKey:     apiKey,
		secretKey:  secretKey,
		passphrase: passphrase,
		restURL:    restURL,
		simulated:  simulated,
		http:       &http.Client{Timeout: 10 * time.Second},
	}
}

// apiResp OKX 响应统一外层结构。
type apiResp struct {
	Code string          `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// request 通用请求入口。signed=true 时附加签名 header。
//
// 这里故意不做"魔法重试"。429 和 50011（rate limit）由调用方自己决定怎么退避。
// 网络错误重试是程序员该想清楚的事，框架替你做就是制造灾难。
func (c *Client) request(method, path string, query map[string]string, body interface{}, signed bool) (json.RawMessage, error) {
	fullPath := path
	if len(query) > 0 {
		fullPath += "?" + encodeQuery(query)
	}

	var bodyStr string
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyStr = string(raw)
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.restURL+fullPath, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.simulated {
		req.Header.Set("x-simulated-trading", "1")
	}

	if signed {
		ts := timestampISO()
		signature := sign(c.secretKey, ts, method, fullPath, bodyStr)
		req.Header.Set("OK-ACCESS-KEY", c.apiKey)
		req.Header.Set("OK-ACCESS-SIGN", signature)
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	var ar apiResp
	if err := json.Unmarshal(raw, &ar); err != nil {
		return nil, fmt.Errorf("decode resp: %w (raw=%s)", err, string(raw))
	}
	if ar.Code != "0" {
		// OKX 批量接口（包括下单）顶层 code=1 表示"有失败"，真因藏在 data 数组里。
		// 把 data 原文带出来，否则只看顶层会得到一句没意义的 "All operations failed"。
		detail := extractFirstFailure(ar.Data)
		if detail != "" {
			return nil, fmt.Errorf("okx error: code=%s msg=%s | %s", ar.Code, ar.Msg, detail)
		}
		return nil, fmt.Errorf("okx error: code=%s msg=%s data=%s",
			ar.Code, ar.Msg, string(ar.Data))
	}
	return ar.Data, nil
}

// extractFirstFailure 从 OKX data 数组里挑第一个 sCode != "0" 的子项作详细错误。
// 形如 [{"sCode":"51008","sMsg":"Order amount ..."}, ...]。
// 不能解析就返回空串，由调用方退回到打印原始 data。
func extractFirstFailure(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var items []struct {
		SCode string `json:"sCode"`
		SMsg  string `json:"sMsg"`
	}
	if err := json.Unmarshal(data, &items); err != nil {
		return ""
	}
	for _, it := range items {
		if it.SCode != "" && it.SCode != "0" {
			return fmt.Sprintf("sCode=%s sMsg=%s", it.SCode, it.SMsg)
		}
	}
	return ""
}

func encodeQuery(q map[string]string) string {
	// 不用 url.Values 是因为 OKX 对部分接口的参数顺序敏感（罕见但有）。
	// 这里手写也方便日后调试。
	buf := bytes.Buffer{}
	first := true
	for k, v := range q {
		if !first {
			buf.WriteByte('&')
		}
		buf.WriteString(k)
		buf.WriteByte('=')
		buf.WriteString(v)
		first = false
	}
	return buf.String()
}
