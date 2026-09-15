package usmarket

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// cboeURL は Cboe 公式の日次四本値（遅延配信の履歴。認証なし、約 10 分ごとに更新）。
const cboeURL = "https://cdn.cboe.com/api/global/delayed_quotes/charts/historical/%s.json"

// cboeSymbols は FRED の系列 ID → Cboe の銘柄。終値は FRED と同じもの（2016-09〜2026-09 の
// 2,515 日で VIX は全日一致、S&P500 は 2019-02-28 の 1 日を除いて 0.003% 以内）。
var cboeSymbols = map[string]string{"SP500": "_SPX", "VIXCLS": "_VIX"}

// closeMargin は NY の 16:00 の引けから、その日の終値を確定とみなすまでの猶予。
const closeMargin = 15 * time.Minute

// CboeFetcher は Cboe から終値を取る。
//
// FRED は前夜の終値を ET の 20 時過ぎ（9:10 JST ごろ）に出すので、9:01 の寄付に間に合わない
// （2026-09-11・09-15 は 9:01〜9:10 の回が前々夜の値で判定していた）。Cboe は引け後すぐに出る。
type CboeFetcher struct {
	client *http.Client
	// now は引けたかの判定に使う時計（テストで差し替える）。
	now func() time.Time
}

// NewCboeFetcher は 1 リクエストの待ち時間を指定して取得元を作る。
func NewCboeFetcher(timeout time.Duration) *CboeFetcher {
	return &CboeFetcher{client: &http.Client{Timeout: timeout}, now: time.Now}
}

// Closes は FRED の系列 ID（SP500 / VIXCLS）の日付 → 終値。
func (f *CboeFetcher) Closes(series string, start, end time.Time) (map[string]float64, error) {
	symbol, ok := cboeSymbols[series]
	if !ok {
		return nil, fmt.Errorf("Cboe に無い系列: %s", series)
	}
	resp, err := f.client.Get(fmt.Sprintf(cboeURL, symbol))
	if err != nil {
		return nil, fmt.Errorf("Cboe %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Cboe %s: HTTP %d", symbol, resp.StatusCode)
	}
	return parseCboe(resp.Body, start, end, f.now())
}

type cboeHistory struct {
	Data []struct {
		Date  string `json:"date"`
		Close string `json:"close"`
	} `json:"data"`
}

// parseCboe は応答 → 日付 → 終値（start〜end、日付で比べる）。引けていない日の行は入れない
// （米国の取引時間中に取ると当日の途中の値が載っている）。
func parseCboe(r io.Reader, start, end, now time.Time) (map[string]float64, error) {
	var body cboeHistory
	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return nil, fmt.Errorf("Cboe の応答が読めない: %w", err)
	}
	from, to := start.Format(dateLayout), end.Format(dateLayout)
	out := make(map[string]float64)
	for _, row := range body.Data {
		if row.Date < from || row.Date > to || !sessionClosed(row.Date, now) {
			continue
		}
		v, err := strconv.ParseFloat(row.Close, 64)
		if err != nil || v <= 0 {
			continue
		}
		out[row.Date] = v
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Cboe の応答に %s〜%s の終値がありません", from, to)
	}
	return out, nil
}

// sessionClosed は NY の日付 date（YYYY-MM-DD）の引け（16:00 ET + 猶予）を now が過ぎたか。
func sessionClosed(date string, now time.Time) bool {
	d, err := time.Parse(dateLayout, date)
	if err != nil {
		return false
	}
	close := time.Date(d.Year(), d.Month(), d.Day(), 16, 0, 0, 0, newYork).Add(closeMargin)
	return !now.Before(close)
}
