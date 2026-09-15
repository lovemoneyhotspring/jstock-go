package usmarket

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// yahooBaseURL は Yahoo Finance の chart API（非公式。認証なし）。
const yahooBaseURL = "https://query1.finance.yahoo.com/v8/finance/chart/"

// yahooSymbols は FRED の系列 ID → Yahoo の銘柄（URL エンコード済み）。
var yahooSymbols = map[string]string{"SP500": "%5EGSPC", "VIXCLS": "%5EVIX"}

// yahooUserAgent は送る User-Agent。Go の既定（Go-http-client/1.1）と空は 429 で弾かれる
// （2026-09-15 に確認）。
const yahooUserAgent = "Mozilla/5.0"

// YahooFetcher は Yahoo Finance から終値を取る。Cboe が落ちた朝の予備
// （FRED は前夜の値が 9:10 JST ごろまで出ないので、寄付の予備にならない）。
//
// 値の精度は Cboe より落ちる: FRED のキャッシュと 2016-09〜2026-09 の 2,515 日を突き合わせ、
// S&P500 は 7 日（最大 0.04%）、VIX は 3 日ずれた（2026-02-06 は FRED・Cboe の 17.76 に対し 20.37）。
// ずれた日はすべて FRED と Cboe が一致していた。だから Cboe の後ろに置く。
type YahooFetcher struct {
	client *http.Client
	// baseURL は取得先（テストで差し替える）。
	baseURL string
	// now は引けたかの判定に使う時計（テストで差し替える）。
	now func() time.Time
}

// NewYahooFetcher は 1 リクエストの待ち時間を指定して取得元を作る。
func NewYahooFetcher(timeout time.Duration) *YahooFetcher {
	return &YahooFetcher{client: &http.Client{Timeout: timeout}, baseURL: yahooBaseURL, now: time.Now}
}

// Closes は FRED の系列 ID（SP500 / VIXCLS）の日付 → 終値。
func (f *YahooFetcher) Closes(series string, start, end time.Time) (map[string]float64, error) {
	symbol, ok := yahooSymbols[series]
	if !ok {
		return nil, fmt.Errorf("Yahoo に無い系列: %s", series)
	}
	// period は UTC の秒。end の日のセッション（NY の 9:30 始まり）を含むよう 2 日先まで取る
	sy, sm, sd := start.Date()
	ey, em, ed := end.Date()
	period1 := time.Date(sy, sm, sd, 0, 0, 0, 0, time.UTC).Unix()
	period2 := time.Date(ey, em, ed, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 2).Unix()
	url := fmt.Sprintf("%s%s?period1=%d&period2=%d&interval=1d", f.baseURL, symbol, period1, period2)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", yahooUserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Yahoo %s: %w", series, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Yahoo %s: HTTP %d", series, resp.StatusCode)
	}
	return parseYahoo(resp.Body, start, end, f.now())
}

type yahooChart struct {
	Chart struct {
		Result []struct {
			Meta struct {
				ExchangeTimezoneName string `json:"exchangeTimezoneName"`
				GMTOffset            int    `json:"gmtoffset"`
			} `json:"meta"`
			Timestamp  []int64 `json:"timestamp"`
			Indicators struct {
				Quote []struct {
					Close []*float64 `json:"close"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
		Error *struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		} `json:"error"`
	} `json:"chart"`
}

// parseYahoo は応答 → 日付 → 終値（start〜end、日付で比べる）。
//
// timestamp はその日の取引の始まり（S&P500 は NY の 9:30、VIX は Chicago の深夜）なので、
// 取引所の時間帯で日付にする。終値が null の行（VIX で年に数日ある）と、引けていない日の行は入れない。
func parseYahoo(r io.Reader, start, end, now time.Time) (map[string]float64, error) {
	var body yahooChart
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("Yahoo の応答が読めない: %w", err)
	}
	if e := body.Chart.Error; e != nil {
		return nil, fmt.Errorf("Yahoo: %s %s", e.Code, e.Description)
	}
	if len(body.Chart.Result) == 0 || len(body.Chart.Result[0].Indicators.Quote) == 0 {
		return nil, fmt.Errorf("Yahoo の応答に終値がありません")
	}
	result := body.Chart.Result[0]
	loc, err := time.LoadLocation(result.Meta.ExchangeTimezoneName)
	if err != nil || result.Meta.ExchangeTimezoneName == "" {
		loc = time.FixedZone("exchange", result.Meta.GMTOffset)
	}
	closes := result.Indicators.Quote[0].Close
	from, to := start.Format(dateLayout), end.Format(dateLayout)
	out := make(map[string]float64)
	for i, ts := range result.Timestamp {
		if i >= len(closes) {
			break
		}
		if closes[i] == nil || *closes[i] <= 0 {
			continue
		}
		date := time.Unix(ts, 0).In(loc).Format(dateLayout)
		if date < from || date > to || !sessionClosed(date, now) {
			continue
		}
		out[date] = *closes[i]
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Yahoo の応答に %s〜%s の終値がありません", from, to)
	}
	return out, nil
}
