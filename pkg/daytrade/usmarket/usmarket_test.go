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
