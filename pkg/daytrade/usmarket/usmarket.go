// Package usmarket は前夜の米国市場（S&P500・VIX）。寄付前に分かる危険信号の材料。
//
// 米国の引けは 5:00〜6:00 JST。9:00 の open はキャッシュに前夜ぶんが無いときだけ取りに行く
// （LatestBeforeCached。寄付の判断に取得元の遅さを持ち込まない）。
// 取得元は、寄付は Cboe → 落ちていれば Yahoo Finance → FRED（SP500 / VIXCLS。前夜の値は
// 9:10 JST ごろで寄付の早い回には間に合わない）、バックテストは FRED。取れなければ nil を返し、ゲートは効かない。
// バックテスト用に data/daytrade/us.json へ溜める（取得元から取り直せるので data 側）。
package usmarket

import (
	"encoding/json"
	"errors"
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
//
// Vix は取れなかった日は 0。**0 はキャッシュに書かない**（omitempty）——書くと
// 「VIX が 0」という値として残り、次の回が取り直さなくなる（2026-09-16。S&P500 だけ
// 取れて VIX が落ちた朝に 0 が焼き付き、その日は一度も VIX が入らなかった）。
type closes struct {
	Date string  `json:"date"`
	Spx  float64 `json:"spx"`
	Vix  float64 `json:"vix,omitempty"`
}

// Fetcher は終値の取得元。テストで差し替えられるようにインターフェースにする
// （ネットワークに繋ぐテストは書かない）。
type Fetcher interface {
	Closes(series string, start, end time.Time) (map[string]float64, error)
}

// FredFetcher は FRED（wbcore/data）から終値を取る。
type FredFetcher struct{ Provider *data.FREDProvider }

// NewFredFetcher は既定のタイムアウト（15 秒）で取得元を作る。
func NewFredFetcher() *FredFetcher { return NewFredFetcherWithTimeout(15 * time.Second) }

// NewFredFetcherWithTimeout は 1 リクエストの待ち時間を指定して取得元を作る。
// 寄付の判断（9:01）では短く、前夜の温め直しでは長く。
func NewFredFetcherWithTimeout(timeout time.Duration) *FredFetcher {
	return &FredFetcher{Provider: data.NewFREDProvider(timeout)}
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

// firstOf は取得元を順に試す Fetcher。
type firstOf []Fetcher

// FirstOf は取得元を順に試し、最初に取れたものを返す（寄付は Cboe → Yahoo → FRED）。
func FirstOf(fetchers ...Fetcher) Fetcher { return firstOf(fetchers) }

// Closes は最初に取れた取得元の終値。全部だめならそれぞれのエラーをまとめて返す。
func (fs firstOf) Closes(series string, start, end time.Time) (map[string]float64, error) {
	var errs []error
	for _, f := range fs {
		out, err := f.Closes(series, start, end)
		if err == nil && len(out) > 0 {
			return out, nil
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("どの取得元にも %s の終値がありません", series)
	}
	return nil, errors.Join(errs...)
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

// 取得元の呼び方（LatestBeforeCached の source）。
const (
	// SourceCache はキャッシュに day−1 のセッションがあり、取りに行かなかった。
	SourceCache = "cache"
	// SourceFetched は取りに行って取れた（キャッシュも更新した）。
	SourceFetched = "fetched"
	// SourceCacheFallback は取りに行って失敗し、キャッシュの最新（day−1 より古い）で代用した。
	SourceCacheFallback = "cache_fallback"
	// SourceCacheNoVix は前夜の S&P500 はキャッシュにあり、VIX だけどこからも取れなかった。
	SourceCacheNoVix = "cache_no_vix"
)

// LatestBeforeCached は LatestBefore のキャッシュ付き。寄付の判断はこちらを使う。
//
// 9:01 に取りに行くと、遅い日は待ち時間 × 2 本を寄付の判断に上乗せする。そこで
//
//  1. キャッシュに前夜（ExpectedSession）のセッションが **VIX ごと** あればそれを返す
//     （それより新しいものは無い）
//  2. 無ければ取りに行き、取れたらキャッシュに足して返す
//  3. 取れなければキャッシュの最新（day−1 以前）で代用し、エラーも返す（呼び出し側がログに）
//
// VIX が欠けている回を 1 で返さないのは、S&P500 だけ取れた朝にそこで止まると、その日は
// 二度と VIX が入らないため（2026-09-16。9:01 の 0 が 9:13 まで残り、5 回とも VIX 無しで判定した）。
//
// 返したセッションが前夜のものかは呼び出し側が IsFresh で確かめる（取得元の公開が遅れた朝は
// 前々夜の値が返る）。
func LatestBeforeCached(f Fetcher, cachePath string, day time.Time) (*Session, string, error) {
	limit := day.AddDate(0, 0, -1)
	cached, _ := readCache(cachePath)
	if s := latestAtOrBefore(SessionsFrom(cached), limit); IsFresh(s, day) && s.Vix > 0 {
		return s, SourceCache, nil
	}
	// 前夜の S&P500 はキャッシュにあって VIX だけ欠けている回は、**VIX だけ**取り直す。
	// 全部取り直すと、VIX がどこからも取れない朝は 9:01〜9:13 の 5 回とも 3 段のフォールバック
	// （Cboe → Yahoo → FRED、各 8 秒）を走らせ、寄付の判断の直列路に最悪 24 秒が乗る。
	// とくに最後の砦の 9:13 で待つのが一番まずい（2026-09-16 のレビュー）
	if s := latestAtOrBefore(SessionsFrom(cached), limit); IsFresh(s, day) {
		if vix, verr := f.Closes("VIXCLS", day.AddDate(0, 0, -14), limit); verr == nil && len(vix) > 0 {
			merged := mergeVix(cached, vix)
			writeCache(cachePath, merged)
			if s := latestAtOrBefore(SessionsFrom(merged), limit); IsFresh(s, day) && s.Vix > 0 {
				return s, SourceFetched, nil
			}
		}
		// VIX が取れなかった。ここで下の download に落ちると、同じ VIXCLS をもう一度フルで
		// 取りに行って 3 段のフォールバックが 2 周し、待ちが倍（最悪 48 秒）になる——上で
		// 減らしたぶんが帳消しになる。前夜の S&P500 は手元にあるので VIX 無しのまま返し、
		// 次の回にまた VIX だけ試す（2026-09-16 のレビュー）
		return s, SourceCacheNoVix, nil
	}
	rows, err := download(f, day.AddDate(0, 0, -14), limit)
	if err != nil {
		if s := latestAtOrBefore(SessionsFrom(cached), limit); s != nil {
			return s, SourceCacheFallback, err
		}
		return nil, "", err
	}
	merged := mergeCloses(cached, rows)
	writeCache(cachePath, merged)
	return latestAtOrBefore(SessionsFrom(merged), limit), SourceFetched, nil
}

// mergeVix はキャッシュの行に VIX だけを足す（S&P500 は取り直さない）。
// 0 では上書きしない——mergeCloses と同じ約束。
func mergeVix(rows []closes, vix map[string]float64) []closes {
	out := make([]closes, 0, len(rows))
	for _, r := range rows {
		if r.Vix <= 0 {
			if v, ok := vix[r.Date]; ok && v > 0 {
				r.Vix = v
			}
		}
		out = append(out, r)
	}
	return out
}

// latestAtOrBefore は limit 以前で最新のセッション。無ければ nil。
func latestAtOrBefore(sessions []Session, limit time.Time) *Session {
	for i := len(sessions) - 1; i >= 0; i-- {
		if !sessions[i].Date.After(limit) {
			s := sessions[i]
			return &s
		}
	}
	return nil
}

// mergeCloses は 2 つの終値の並びを日付で重ね、日付順にする（新しい方が勝つ）。
// ただし **VIX は 0（取れなかった）で上書きしない**——S&P500 だけ取れた回が、前に
// 取れていた VIX を消してしまう（2026-09-16）。
func mergeCloses(old, fresh []closes) []closes {
	byDate := make(map[string]closes, len(old)+len(fresh))
	for _, r := range old {
		byDate[r.Date] = r
	}
	for _, r := range fresh {
		if r.Vix <= 0 {
			if prev, ok := byDate[r.Date]; ok && prev.Vix > 0 {
				r.Vix = prev.Vix
			}
		}
		byDate[r.Date] = r
	}
	out := make([]closes, 0, len(byDate))
	for _, r := range byDate {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b closes) int { return cmpString(a.Date, b.Date) })
	return out
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
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
