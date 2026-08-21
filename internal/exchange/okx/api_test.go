package okx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetKlinesDropsUnconfirmedCandle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"code":"0",
			"msg":"",
			"data":[
				["2000","101","103","100","102","10","0","0","0"],
				["1000","99","102","98","101","20","0","0","1"]
			]
		}`)
	}))
	defer server.Close()

	client := NewClient("", "", "", server.URL, false)
	klines, err := client.GetKlines("ETH-USDT-SWAP", "1D", 2)
	if err != nil {
		t.Fatalf("GetKlines: %v", err)
	}
	if len(klines) != 1 {
		t.Fatalf("应只保留 1 根已收盘 K 线，实际得到 %d 根", len(klines))
	}
	if klines[0].Timestamp != 1000 || klines[0].Close != 101 {
		t.Fatalf("保留了错误的 K 线：%+v", klines[0])
	}
}
