package usmarket

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// stub は取得元の代わり。ネットワークには繋がない。
type stub struct {
	closes map[string]map[string]float64
	calls  int
}

func (s *stub) Closes(series string, start, end time.Time) (map[string]float64, error) {
	s.calls++
	if s.closes[series] == nil {
		return nil, fmt.Errorf("系列がありません: %s", series)
	}
	return s.closes[series], nil
}

func newStub() *stub {
	return &stub{closes: map[string]map[string]float64{
		"SP500": {
			"2026-09-01": 5000,
			"2026-09-02": 5025, // +0.5%（小幅高の帯に入る）
			"2026-09-03": 5075, // +1.0%
		},
		"VIXCLS": {"2026-09-01": 14, "2026-09-02": 15, "2026-09-03": 16},
	}}
}

func day(iso string) time.Time {
	t, _ := time.Parse("2006-01-02", iso)
	return t
}

func TestSessionsFromComputesReturns(t *testing.T) {
	sessions, err := History(newStub(), "", day("2026-09-01"), day("2026-09-03"))
	if err != nil {
		t.Fatal(err)
	}
	// 初日はリターンが取れないので 2 日ぶん
	if len(sessions) != 2 {
		t.Fatalf("セッション %d 件, want 2", len(sessions))
	}
	if r := sessions[0].SpxRet; r < 0.0049 || r > 0.0051 {
		t.Errorf("リターン = %v, want ≈0.005", r)
	}
	if sessions[0].Vix != 15 {
		t.Errorf("VIX = %v", sessions[0].Vix)
	}
}

func TestLatestBeforeUsesPreviousSession(t *testing.T) {
	// 判定日 09-03 の寄付前に確定しているのは NY の 09-02
	session, err := LatestBefore(newStub(), day("2026-09-03"))
	if err != nil || session == nil {
		t.Fatalf("session = %v, %v", session, err)
	}
	if session.Date.Format("2006-01-02") != "2026-09-02" {
		t.Errorf("当日の米国を先読みしている: %s", session.Date)
	}
}

func TestAsOfSkipsStaleSessions(t *testing.T) {
	sessions := []Session{
		{Date: day("2026-09-02"), SpxRet: 0.005, Vix: 15},
	}
	got := AsOf(sessions, []time.Time{day("2026-09-03"), day("2026-09-20")})
	if _, ok := got["2026-09-03"]; !ok {
		t.Error("前日のセッションを当てていない")
	}
	// 連休を跨いだ古い値では判断しない
	if _, ok := got["2026-09-20"]; ok {
		t.Error("2 週間以上前のセッションを当てている")
	}
}

func TestHistoryUsesCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	source := newStub()
	if _, err := History(source, cache, day("2026-09-01"), day("2026-09-03")); err != nil {
		t.Fatal(err)
	}
	first := source.calls
	if _, err := History(source, cache, day("2026-09-01"), day("2026-09-03")); err != nil {
		t.Fatal(err)
	}
	if source.calls != first {
		t.Errorf("キャッシュがあるのに取り直している: %d → %d", first, source.calls)
	}
}

func TestFetchFailureIsAnError(t *testing.T) {
	// 取得元の障害は呼び出し側が握る（ゲートを効かせないだけで寄付は止めない）
	empty := &stub{closes: map[string]map[string]float64{}}
	if _, err := LatestBefore(empty, day("2026-09-03")); err == nil {
		t.Error("取得失敗がエラーにならない")
	}
}

func TestDefaultCachePath(t *testing.T) {
	if got := DefaultCachePath("data"); got != filepath.Join("data", "daytrade", "us.json") {
		t.Errorf("キャッシュの置き場 = %s", got)
	}
}

// TestLatestBeforeCachedSkipsFetchWhenCacheHasYesterday は、キャッシュに day−1 があれば取りに行かないこと。
func TestLatestBeforeCachedSkipsFetchWhenCacheHasYesterday(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	source := newStub()
	// 前夜の温め: 9/4 の判断に要る 9/3 まで取る
	s, src, err := LatestBeforeCached(source, cache, day("2026-09-04"))
	if err != nil || s == nil || src != SourceFetched || !s.Date.Equal(day("2026-09-03")) {
		t.Fatalf("初回: %+v %s %v", s, src, err)
	}
	calls := source.calls
	// 朝: キャッシュに 9/3 があるので取りに行かない
	s, src, err = LatestBeforeCached(source, cache, day("2026-09-04"))
	if err != nil || s == nil || src != SourceCache || !s.Date.Equal(day("2026-09-03")) {
		t.Fatalf("2 回目: %+v %s %v", s, src, err)
	}
	if source.calls != calls {
		t.Errorf("キャッシュがあるのに取りに行った: %d → %d", calls, source.calls)
	}
}

// TestLatestBeforeCachedHitsCacheAfterHoliday は、祝日明けは前夜＝最後の取引日としてキャッシュを使うこと。
func TestLatestBeforeCachedHitsCacheAfterHoliday(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	source := newStub()
	source.closes["SP500"]["2026-09-04"] = 5100
	source.closes["VIXCLS"]["2026-09-04"] = 17 // VIX ごと揃っていればキャッシュで済ませる
	if _, _, err := LatestBeforeCached(source, cache, day("2026-09-05")); err != nil {
		t.Fatal(err)
	}
	calls := source.calls
	// 9/8（火）の前夜は労働者の日（9/7）を飛ばして 9/4（金）
	s, src, err := LatestBeforeCached(source, cache, day("2026-09-08"))
	if err != nil || src != SourceCache || !IsFresh(s, day("2026-09-08")) {
		t.Fatalf("祝日明け: %+v %s %v", s, src, err)
	}
	if source.calls != calls {
		t.Errorf("前夜の値がキャッシュにあるのに取りに行った: %d → %d", calls, source.calls)
	}
}

// TestFirstOfFallsBack は、先の取得元が落ちていれば次を使い、全部だめならエラーにすること。
func TestFirstOfFallsBack(t *testing.T) {
	broken := &stub{closes: map[string]map[string]float64{}}
	got, err := FirstOf(broken, newStub()).Closes("SP500", day("2026-09-01"), day("2026-09-03"))
	if err != nil || got["2026-09-03"] != 5075 {
		t.Errorf("次の取得元に回らない: %v %v", got, err)
	}
	if _, err := FirstOf(broken, broken).Closes("SP500", day("2026-09-01"), day("2026-09-03")); err == nil {
		t.Error("全部落ちているのにエラーにならない")
	}
}

// TestUntilSkipsSourcesAfterDeadline は、締め切りを過ぎたら取得元に繋がずに落とすこと
// （寄る前の回が米国市場の取得だけで窓を使い切らないため）。前なら素通しする。
func TestUntilSkipsSourcesAfterDeadline(t *testing.T) {
	late := newStub()
	if _, err := FirstOf(Until(time.Now().Add(-time.Second), late)...).Closes("SP500", day("2026-09-01"), day("2026-09-03")); err == nil {
		t.Error("締め切りを過ぎているのにエラーにならない")
	}
	if late.calls != 0 {
		t.Errorf("締め切りを過ぎているのに取得元を呼んだ: %d 回", late.calls)
	}
	early := newStub()
	got, err := FirstOf(Until(time.Now().Add(time.Minute), early)...).Closes("SP500", day("2026-09-01"), day("2026-09-03"))
	if err != nil || got["2026-09-03"] != 5075 || early.calls != 1 {
		t.Errorf("締め切りの前なのに取れない: %v %v（%d 回）", got, err, early.calls)
	}
}

// TestFirstOfFillsMissingLatestDay は、先の取得元が前夜の行だけ欠いていれば次の取得元で
// その日を補い、先の取得元の値は上書きしないこと（2026-09-17。Cboe の VIX が 9/16 を
// まだ載せておらず、Yahoo に回らなかった）。
func TestFirstOfFillsMissingLatestDay(t *testing.T) {
	cboe := &stub{closes: map[string]map[string]float64{
		"VIXCLS": {"2026-09-01": 14, "2026-09-02": 15},
	}}
	yahoo := &stub{closes: map[string]map[string]float64{
		"VIXCLS": {"2026-09-01": 99, "2026-09-02": 99, "2026-09-03": 16},
	}}
	fred := newStub()
	got, err := FirstOf(cboe, yahoo, fred).Closes("VIXCLS", day("2026-09-01"), day("2026-09-03"))
	if err != nil || got["2026-09-03"] != 16 {
		t.Fatalf("前夜の VIX を次の取得元で補わない: %v %v", got, err)
	}
	if got["2026-09-01"] != 14 || got["2026-09-02"] != 15 {
		t.Errorf("先の取得元の値を上書きした: %v", got)
	}
	if fred.calls != 0 {
		t.Errorf("揃った後も次の取得元に聞いた: %d 回", fred.calls)
	}

	// どこにも前夜の行が無ければ、手元の最新までを返す（エラーにしない）。
	got, err = FirstOf(cboe, cboe).Closes("VIXCLS", day("2026-09-01"), day("2026-09-03"))
	if err != nil || len(got) != 2 {
		t.Errorf("前夜の行がどこにも無い回: %v %v", got, err)
	}
}

// TestLatestBeforeCachedFillsVixFromSecondSource は、キャッシュに前夜の S&P500 だけがあり、
// 先の取得元の VIX が前夜を欠く朝に、次の取得元の VIX で埋めて返すこと。
func TestLatestBeforeCachedFillsVixFromSecondSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "us.json")
	writeCache(path, []closes{
		{Date: "2026-09-02", Spx: 5025, Vix: 15},
		{Date: "2026-09-03", Spx: 5075},
	})
	cboe := &stub{closes: map[string]map[string]float64{
		"VIXCLS": {"2026-09-01": 14, "2026-09-02": 15},
	}}
	s, source, err := LatestBeforeCached(FirstOf(cboe, newStub()), path, day("2026-09-04"))
	if err != nil || s == nil || s.Vix != 16 || source != SourceFetched {
		t.Fatalf("VIX を埋めない: %+v %s %v", s, source, err)
	}
}

// TestLatestBeforeCachedFallsBackToCache は、取りに行って失敗したらキャッシュの最新で代用し、
// エラーも返すこと（ログには残す。判断は止めない）。
func TestLatestBeforeCachedFallsBackToCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	if _, _, err := LatestBeforeCached(newStub(), cache, day("2026-09-04")); err != nil {
		t.Fatal(err)
	}
	// 翌日の朝、取得元が落ちている。キャッシュの 9/3 で代用する
	broken := &stub{closes: map[string]map[string]float64{}}
	s, src, err := LatestBeforeCached(broken, cache, day("2026-09-05"))
	if err == nil {
		t.Error("取得失敗がエラーで伝わらない")
	}
	if s == nil || src != SourceCacheFallback || !s.Date.Equal(day("2026-09-03")) {
		t.Fatalf("代用が効いていない: %+v %s", s, src)
	}
	// キャッシュも無ければ nil
	s, _, err = LatestBeforeCached(broken, filepath.Join(t.TempDir(), "none.json"), day("2026-09-05"))
	if err == nil || s != nil {
		t.Errorf("キャッシュ無し: %+v %v", s, err)
	}
}

// TestLatestBeforeCachedRefetchesWhenVixMissing は、S&P500 だけ取れて VIX が落ちた回の
// キャッシュで止まらず、次の回が取りに行くこと。0 を「値」として残すと、その日は
// 二度と VIX が入らなかった（2026-09-16）。
func TestLatestBeforeCachedRefetchesWhenVixMissing(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	// 1 回目: VIX だけ取れない朝（S&P500 は取れるので取得そのものは成功する）
	noVix := newStub()
	delete(noVix.closes, "VIXCLS")
	s, _, err := LatestBeforeCached(noVix, cache, day("2026-09-04"))
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s.Vix != 0 {
		t.Fatalf("VIX が取れない朝: %+v", s)
	}
	// 2 回目: 取得元が直った。VIX が欠けたキャッシュで済ませず取りに行く
	source := newStub()
	s, src, err := LatestBeforeCached(source, cache, day("2026-09-04"))
	if err != nil {
		t.Fatal(err)
	}
	if src != SourceFetched {
		t.Errorf("VIX が欠けたキャッシュで済ませた: %s", src)
	}
	if s == nil || s.Vix != 16 {
		t.Fatalf("取り直した VIX が入っていない: %+v", s)
	}
}

// TestLatestBeforeCachedStopsAfterVixOnlyRetry は、VIX がどこからも取れない朝に取得を
// 2 周させないこと。VIX だけ取り直して失敗した後にフルの download へ落ちると、同じ VIXCLS を
// もう一度取りに行って 3 段のフォールバックが 2 周し、寄付の直列路に倍の待ちが乗る
// （2026-09-16 のレビュー）。
func TestLatestBeforeCachedStopsAfterVixOnlyRetry(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "us.json")
	// 1 回目: S&P500 だけ取れて VIX が無い。キャッシュには前夜の S&P500 が残る
	noVix := newStub()
	delete(noVix.closes, "VIXCLS")
	if _, _, err := LatestBeforeCached(noVix, cache, day("2026-09-04")); err != nil {
		t.Fatal(err)
	}
	// 2 回目: 前夜の S&P500 はキャッシュにある。取りに行くのは VIXCLS の 1 回だけ
	again := newStub()
	delete(again.closes, "VIXCLS")
	s, src, err := LatestBeforeCached(again, cache, day("2026-09-04"))
	if err != nil {
		t.Fatal(err)
	}
	if again.calls != 1 {
		t.Errorf("取得を %d 回走らせた（VIX だけの 1 回で済むはず）", again.calls)
	}
	if src != SourceCacheNoVix {
		t.Errorf("取得元 = %s, want %s", src, SourceCacheNoVix)
	}
	if s == nil || !IsFresh(s, day("2026-09-04")) || s.Vix != 0 {
		t.Fatalf("前夜の S&P500 を VIX 無しで返していない: %+v", s)
	}
}

// TestMergeClosesKeepsVix は、S&P500 だけ取れた回の 0 で、前に取れていた VIX を消さないこと。
func TestMergeClosesKeepsVix(t *testing.T) {
	old := []closes{{Date: "2026-09-03", Spx: 5075, Vix: 16}}
	fresh := []closes{{Date: "2026-09-03", Spx: 5075}, {Date: "2026-09-04", Spx: 5100}}
	got := mergeCloses(old, fresh)
	if len(got) != 2 {
		t.Fatalf("行数 %d, want 2", len(got))
	}
	if got[0].Vix != 16 {
		t.Errorf("取れていた VIX を 0 で消した: %+v", got[0])
	}
	if got[1].Vix != 0 {
		t.Errorf("無い VIX を作った: %+v", got[1])
	}
}
