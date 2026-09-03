// Package usmarket は前夜の米国市場（S&P500・VIX）。寄付前に分かる危険信号の材料。
//
// 米国の引けは 6:00 JST。9:00 の open が取りに行く（20:30 の plan には無い）。
// 取得元は FRED（SP500 / VIXCLS の日次終値）。取れなければ nil を返し、ゲートは効かない。
// バックテスト用に data/daytrade/us.json へ溜める（取得元から取り直せるので data 側）。
package usmarket

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
)

const dateLayout = "2006-01-02"

// Session は米国のある取引日の要約。
type Session struct {
	Date   time.Time `json:"date"`
	SpxRet float64   `json:"spx_ret"`
	// Vix は VIX 終値。取れなければ 0。
	Vix float64 `json:"vix"`
}

// closes は 1 日ぶんの終値（キャッシュのファイル形式）。
type closes struct {
	Date string  `json:"date"`
	Spx  float64 `json:"spx"`
	Vix  float64 `json:"vix"`
}

// Fetcher は終値の取得元。テストで差し替えられるようにインターフェースにする
// （ネットワークに繋ぐテストは書かない）。
type Fetcher interface {
	Closes(series string, start, end time.Time) (map[string]float64, error)
}

// FredFetcher は FRED（wbcore/data）から終値を取る。
type FredFetcher struct{ Provider *data.FREDProvider }

// NewFredFetcher は既定のタイムアウトで取得元を作る。
func NewFredFetcher() *FredFetcher {
	return &FredFetcher{Provider: data.NewFREDProvider(15 * time.Second)}
}

// Closes は FRED の系列 ID（SP500 / VIXCLS）の日付 → 終値。
func (f *FredFetcher) Closes(series string, start, end time.Time) (map[string]float64, error) {
	bars, err := f.Provider.FetchBars(series, start.Format(dateLayout), end.Format(dateLayout))
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(bars))
	for _, bar := range bars {
		v, _ := bar.Close.Float64()
		if v > 0 {
			out[bar.Date] = v
		}
	}
	return out, nil
}

// download は S&P500 と VIX の終値を日付でそろえる。
func download(f Fetcher, start, end time.Time) ([]closes, error) {
	spx, err := f.Closes("SP500", start, end)
	if err != nil {
		return nil, err
	}
	// VIX が取れなくても S&P500 だけで判定できる（VIX の例外規則が効かなくなるだけ）
	vix, _ := f.Closes("VIXCLS", start, end)

	dates := make([]string, 0, len(spx))
	for d := range spx {
		dates = append(dates, d)
	}
	slices.Sort(dates)
	out := make([]closes, 0, len(dates))
	for _, d := range dates {
		out = append(out, closes{Date: d, Spx: spx[d], Vix: vix[d]})
	}
	return out, nil
}

// SessionsFrom は終値の並び → セッション（前日比のリターンを付ける）。
func SessionsFrom(rows []closes) []Session {
	var out []Session
	for i := 1; i < len(rows); i++ {
		prev, cur := rows[i-1], rows[i]
		if prev.Spx <= 0 || cur.Spx <= 0 {
			continue
		}
		day, err := time.Parse(dateLayout, cur.Date)
		if err != nil {
			continue
		}
		out = append(out, Session{Date: day, SpxRet: cur.Spx/prev.Spx - 1, Vix: cur.Vix})
	}
	return out
}

// History は start〜end の米国セッション。キャッシュが足りなければ取り直す。
func History(f Fetcher, cachePath string, start, end time.Time) ([]Session, error) {
	rows, ok := readCache(cachePath)
	if ok {
		first, _ := time.Parse(dateLayout, rows[0].Date)
		last, _ := time.Parse(dateLayout, rows[len(rows)-1].Date)
		// 末尾は 4 日ぶんの猶予を持つ（週末・祝日で FRED の更新が遅れる）
		if first.After(start) || last.Before(end.AddDate(0, 0, -4)) {
			ok = false
		}
	}
	if !ok {
		fetched, err := download(f, start.AddDate(0, 0, -10), end)
		if err != nil {
			return nil, err
		}
		rows = fetched
		writeCache(cachePath, rows)
	}
	return SessionsFrom(rows), nil
}

// LatestBefore は判定日 day の寄付前に確定している最新の米国セッション（NY 日付 ≤ day−1）。
// 取得元の障害で寄付の判断を止めないよう、失敗は nil で返す（エラーは呼び出し側がログに）。
func LatestBefore(f Fetcher, day time.Time) (*Session, error) {
	rows, err := download(f, day.AddDate(0, 0, -14), day.AddDate(0, 0, -1))
	if err != nil {
		return nil, err
	}
	sessions := SessionsFrom(rows)
	limit := day.AddDate(0, 0, -1)
	for i := len(sessions) - 1; i >= 0; i-- {
		if !sessions[i].Date.After(limit) {
			s := sessions[i]
			return &s, nil
		}
	}
	return nil, nil
}

// AsOf は東証の日付ごとに、前日以前の最新の米国セッションを当てる（バックテスト用）。
// 6 営業日以上前のセッションしか無い日は当てない（連休を跨いだ古い値で判断しない）。
func AsOf(sessions []Session, days []time.Time) map[string]Session {
	out := make(map[string]Session, len(days))
	if len(sessions) == 0 {
		return out
	}
	sorted := slices.Clone(sessions)
	slices.SortFunc(sorted, func(a, b Session) int { return a.Date.Compare(b.Date) })
	for _, day := range days {
		limit := day.AddDate(0, 0, -1)
		tolerance := day.AddDate(0, 0, -7)
		for i := len(sorted) - 1; i >= 0; i-- {
			if sorted[i].Date.After(limit) {
				continue
			}
			if sorted[i].Date.Before(tolerance) {
				break
			}
			out[day.Format(dateLayout)] = sorted[i]
			break
		}
	}
	return out
}

// DefaultCachePath は data ディレクトリ配下の置き場。
func DefaultCachePath(dataDir string) string {
	return filepath.Join(dataDir, "daytrade", "us.json")
}

func readCache(path string) ([]closes, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var rows []closes
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

func writeCache(path string, rows []closes) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return
	}
	// 一時ファイルに書いて rename（途中で落ちても壊れたキャッシュを残さない）
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Describe はログ用の 1 行。
func (s Session) Describe() string {
	return fmt.Sprintf("%s S&P500 %+.2f%% VIX %.1f", s.Date.Format(dateLayout), s.SpxRet*100, s.Vix)
}
