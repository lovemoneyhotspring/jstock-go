package usmarket

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// yahooSPX は ^GSPC の応答の抜粋（timestamp は NY の 9:30 = 9/10・9/11・9/14、2026-09-15 に取得）。
const yahooSPX = `{"chart":{"result":[{"meta":{"exchangeTimezoneName":"America/New_York","gmtoffset":-14400},
"timestamp":[1789047000,1789133400,1789392600],
"indicators":{"quote":[{"close":[7591.7001953125,7656.97998046875,7619.97998046875]}]}}],"error":null}}`

// yahooVIX は ^VIX の応答の抜粋（timestamp は Chicago の深夜 = 9/11・9/14。null の行を 1 本混ぜる）。
const yahooVIX = `{"chart":{"result":[{"meta":{"exchangeTimezoneName":"America/Chicago","gmtoffset":-18000},
"timestamp":[1789110000,1789196400,1789369200],
"indicators":{"quote":[{"close":[15.84000015258789,null,17.100000381469727]}]}}],"error":null}}`

func TestParseYahoo(t *testing.T) {
	// 9:01 JST の 9/15 = NY の 9/14 20:01（引け後）
	now := time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC)
	spx, err := parseYahoo(strings.NewReader(yahooSPX), day("2026-09-11"), day("2026-09-14"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(spx) != 2 || spx["2026-09-14"] < 7619.97 || spx["2026-09-14"] > 7619.99 {
		t.Errorf("S&P500 = %v（範囲外の 9/10 は入れない）", spx)
	}
	vix, err := parseYahoo(strings.NewReader(yahooVIX), day("2026-09-10"), day("2026-09-14"), now)
	if err != nil {
		t.Fatal(err)
	}
	// Chicago の深夜の timestamp を UTC で日付にすると前日にずれる。取引所の時間帯で読む
	if len(vix) != 2 || vix["2026-09-11"] == 0 || vix["2026-09-14"] == 0 {
		t.Errorf("VIX = %v（null の行は入れない）", vix)
	}
}

func TestParseYahooSkipsSessionInProgress(t *testing.T) {
	// NY の 9/14 15:30（取引時間中）に取ると 9/14 の行は途中の値
	now := time.Date(2026, 9, 14, 19, 30, 0, 0, time.UTC)
	got, err := parseYahoo(strings.NewReader(yahooSPX), day("2026-09-10"), day("2026-09-14"), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["2026-09-14"]; ok {
		t.Error("引けていない日の値を終値として入れた")
	}
}

func TestParseYahooErrors(t *testing.T) {
	now := time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC)
	for name, body := range map[string]string{
		"エラー応答":   `{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`,
		"範囲に行が無い": yahooSPX,
		"読めない":    "<html>Too Many Requests</html>",
	} {
		from, to := day("2026-09-01"), day("2026-09-15")
		if name == "範囲に行が無い" {
			from, to = day("2026-08-01"), day("2026-08-31")
		}
		if _, err := parseYahoo(strings.NewReader(body), from, to, now); err == nil {
			t.Errorf("%s: エラーにならない", name)
		}
	}
}

// TestYahooFetcherSendsUserAgent は User-Agent を付けて期間つきで取りに行くこと
// （Go の既定の User-Agent だと 429 で弾かれる）。
func TestYahooFetcherSendsUserAgent(t *testing.T) {
	var gotUA, gotPath, gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotPath, gotQuery = r.UserAgent(), r.URL.EscapedPath(), r.URL.RawQuery
		if !strings.HasPrefix(gotUA, "Mozilla/") {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(yahooSPX))
	}))
	defer server.Close()

	f := NewYahooFetcher(5 * time.Second)
	f.baseURL = server.URL + "/v8/finance/chart/"
	f.now = func() time.Time { return time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC) }
	got, err := f.Closes("SP500", day("2026-09-01"), day("2026-09-14"))
	if err != nil {
		t.Fatalf("取得に失敗: %v（UA=%q）", err, gotUA)
	}
	if got["2026-09-14"] == 0 {
		t.Errorf("9/14 の終値が無い: %v", got)
	}
	if gotPath != "/v8/finance/chart/%5EGSPC" || !strings.Contains(gotQuery, "interval=1d") {
		t.Errorf("取得先 = %s?%s", gotPath, gotQuery)
	}
	if _, err := f.Closes("DGS10", day("2026-09-01"), day("2026-09-14")); err == nil {
		t.Error("知らない系列がエラーにならない")
	}
}
