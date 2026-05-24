package okx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"time"
)

// OKX V5 签名规则:
//   sign = base64( HMAC-SHA256( secretKey, timestamp + method + requestPath + body ) )
//   timestamp 格式: ISO8601 毫秒, e.g. 2020-12-08T09:08:57.715Z
//   method 必须大写: GET / POST
//   requestPath 包括 query string
//   GET 请求 body 为空字符串
func sign(secret, timestamp, method, path, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + method + path + body))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// timestampISO 返回 OKX 要求的 ISO8601 毫秒时间戳。
func timestampISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
