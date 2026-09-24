package news

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

func countWhere(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 訂正で銘柄・ジャンルが外れた記事は、転置からも外れる。
func TestSaveReplacesInvertedRows(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "2026-09-11", items(), time.Now()); err != nil {
		t.Fatal(err)
	}
	fixed := items()
	fixed[0].Codes = []string{"9432"}         // 7203 を外した
	fixed[0].Genres = []string{"62199"}       // 3001 を外した
	fixed[1].Codes = []string{"7203", "6758"} // 足した
	if _, err := s.Save(ctx, "2026-09-11", fixed, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM news_codes WHERE news_id = '1' AND code = '7203'`); n != 0 {
		t.Errorf("外した銘柄が残っている: %d", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM news_genres WHERE news_id = '1' AND genre = '3001'`); n != 0 {
		t.Errorf("外したジャンルが残っている: %d", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM news_codes WHERE news_id = '2'`); n != 2 {
		t.Errorf("記事 2 の銘柄 = %d, want 2", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM news_codes WHERE code = '7203'`); n != 1 {
		t.Errorf("7203 の記事数 = %d, want 1", n)
	}
}

// new_items は「その回に初めて見た件数」。取り直しで足し込まない。同じ秒の保存でも数え直さない。
// items は取り直しの短い応答で減らさない。
func TestSaveNewItemsAndItemsCount(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 14, 6, 30, 0, 0, time.UTC)
	dayRow := func() (int, int) {
		t.Helper()
		var items, newItems int
		if err := s.DB().QueryRow(`SELECT items, new_items FROM news_days WHERE feed_date = '2026-09-11'`).
			Scan(&items, &newItems); err != nil {
			t.Fatal(err)
		}
		return items, newItems
	}
	if n, err := s.Save(ctx, "2026-09-11", items(), at); err != nil || n != 2 {
		t.Fatalf("初回 = %d %v", n, err)
	}
	// 同じ秒にもう一度（旧実装は saved_at が同じなので 2 件を初出に数えた）
	if n, err := s.Save(ctx, "2026-09-11", items(), at); err != nil || n != 0 {
		t.Fatalf("同じ秒の 2 回目 = %d %v", n, err)
	}
	if got, newItems := dayRow(); got != 2 || newItems != 0 {
		t.Errorf("items=%d new_items=%d, want 2 / 0", got, newItems)
	}
	// 1 件足された回
	more := append(items(), broker.NewsItem{ID: "3", Time: "1600", Headline: "追記"})
	if n, err := s.Save(ctx, "2026-09-11", more, at.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("追記 = %d %v", n, err)
	}
	if got, newItems := dayRow(); got != 3 || newItems != 1 {
		t.Errorf("items=%d new_items=%d, want 3 / 1", got, newItems)
	}
	// 一時的に短い応答（1 件）でも items は減らない
	if _, err := s.Save(ctx, "2026-09-11", items()[:1], at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := dayRow(); got != 3 {
		t.Errorf("短い応答で items が減った: %d", got)
	}
	// 同じ応答に同じ記事が 2 度あっても 1 件
	dup := []broker.NewsItem{{ID: "9", Time: "0900"}, {ID: "9", Time: "0901"}}
	if n, err := s.Save(ctx, "2026-09-12", dup, at); err != nil || n != 1 {
		t.Errorf("重複 = %d %v", n, err)
	}
}

// 0 件の日（休日）も ok なら済み。
func TestDoneDaysIncludesEmptyOKDays(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "2026-09-13", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure(ctx, "2026-09-12", time.Now(), "timeout"); err != nil {
		t.Fatal(err)
	}
	done, err := s.DoneDays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !done["2026-09-13"] || done["2026-09-12"] {
		t.Errorf("DoneDays = %v", done)
	}
}

// 日が明ける前にしか取れていない日は済みにしない（夜の配信が抜けたままになる）。
// 明けてから取れば済み。
func TestDoneDaysRequiresFetchAfterDayEnd(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	jst := time.FixedZone("JST", 9*3600)
	if _, err := s.Save(ctx, "2026-09-14", items(), time.Date(2026, 9, 14, 20, 20, 0, 0, jst)); err != nil {
		t.Fatal(err)
	}
	// UTC で書いた、日が明けた後（9/15 0:10 JST）の取り込み
	if _, err := s.Save(ctx, "2026-09-13", nil, time.Date(2026, 9, 14, 15, 10, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	done, err := s.DoneDays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if done["2026-09-14"] || !done["2026-09-13"] {
		t.Errorf("DoneDays = %v, want 9/13 だけ済み", done)
	}
}

type stubSource struct {
	byDay map[string][]broker.NewsItem
	fail  map[string]bool
	calls []string
}

func (s *stubSource) News(day string) ([]broker.NewsItem, error) {
	s.calls = append(s.calls, day)
	if s.fail[day] {
		return nil, errors.New("電文の失敗")
	}
	return s.byDay[day], nil
}

// 直近 recent 日は済みでも取り直す。失敗した日は次の回に取り直す。
// recent より古い 0 件の日（休日）は取り直さない。
func TestSync(t *testing.T) {
	clock.Now = func() time.Time { return time.Date(2026, 9, 15, 6, 20, 0, 0, clock.Tokyo) }
	t.Cleanup(func() { clock.Now = time.Now })
	s := openTemp(t)
	ctx := context.Background()
	src := &stubSource{
		byDay: map[string][]broker.NewsItem{
			"20260915": items(), "20260914": items(), "20260913": items(), "20260912": items(),
			// 9/11 は 0 件（休日のつもり）
		},
		fail: map[string]bool{"20260912": true},
	}
	res, err := Sync(ctx, s, src, 5, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Imported != 4 || res.Failed != 1 || res.Skipped != 0 || len(src.calls) != 5 {
		t.Fatalf("1 回目 res = %+v calls = %v", res, src.calls)
	}
	// 9/12 は土曜なので終了コードには出さない（TestFailureErrorSkipsWeekend）
	if err := res.FailureError(); err != nil {
		t.Errorf("土曜だけの失敗の FailureError = %v, want nil", err)
	}
	var status string
	if err := s.DB().QueryRow(`SELECT status FROM news_days WHERE feed_date = '2026-09-12'`).Scan(&status); err != nil || status != "電文の失敗" {
		t.Errorf("失敗の記録 = %q %v", status, err)
	}

	// 2 回目: 直近 3 日（15・14・13）と、失敗した 12 を取る。0 件だった 11 は済み
	src.calls, src.fail = nil, nil
	res, err = Sync(ctx, s, src, 5, 3, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"20260915", "20260914", "20260913", "20260912"}
	if !reflect.DeepEqual(src.calls, want) || res.Skipped != 1 || res.Failed != 0 {
		t.Fatalf("2 回目 calls = %v res = %+v", src.calls, res)
	}
	if err := res.FailureError(); err != nil {
		t.Errorf("失敗の無い回の FailureError = %v", err)
	}
	if res.Days[3].Day != "2026-09-12" || res.Days[3].New != 2 {
		t.Errorf("取り直した 9/12 = %+v", res.Days[3])
	}
	done, _ := s.DoneDays(ctx)
	if !done["2026-09-12"] {
		t.Error("取り直した日が済みになっていない")
	}
}

func TestListQuery(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.Save(ctx, "2026-09-11", items(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(ctx, "2026-09-12", []broker.NewsItem{
		{ID: "5", Time: "0800", Genres: []string{"3001"}, Codes: []string{"9432"}, Headline: "通信", Body: "公開買付の噂"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ids := func(f ListFilter) []string {
		t.Helper()
		query, params := ListQuery(f)
		rows, err := s.DB().QueryContext(ctx, query, params...)
		if err != nil {
			t.Fatalf("%+v: %v", f, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var d, tm, genres, codes, head, body string
			if err := rows.Scan(&d, &tm, &genres, &codes, &head, &body); err != nil {
				t.Fatal(err)
			}
			out = append(out, d+" "+tm)
		}
		return out
	}
	cases := []struct {
		f    ListFilter
		want []string
	}{
		{ListFilter{}, []string{"2026-09-12 0800", "2026-09-11 1530", "2026-09-11 0900"}},
		{ListFilter{Limit: 1}, []string{"2026-09-12 0800"}},
		{ListFilter{Code: "7203"}, []string{"2026-09-11 1530", "2026-09-11 0900"}},
		{ListFilter{Code: "9432", Genre: "3001"}, []string{"2026-09-12 0800", "2026-09-11 0900"}},
		{ListFilter{Day: "2026-09-11", Genre: "6526"}, []string{"2026-09-11 1530"}},
		{ListFilter{Word: "公開買付"}, []string{"2026-09-12 0800", "2026-09-11 0900"}},
		{ListFilter{Word: "' OR 1=1 --"}, nil}, // 語はプレースホルダで渡る（SQL に混ざらない）
		{ListFilter{Code: "0000"}, nil},
	}
	for _, c := range cases {
		if got := ids(c.f); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ListQuery(%+v) = %v, want %v", c.f, got, c.want)
		}
	}
}

// 平日の日単位の失敗は news sync の終了コードに出す。土日の失敗は出さない
// （記事の無い日曜は一覧の項目の無い応答で毎週失敗になる。2026-09-13・09-20）。
func TestFailureErrorSkipsWeekend(t *testing.T) {
	boom := errors.New("電文の失敗")
	weekend := SyncResult{Failed: 2, Days: []SyncDay{
		{Day: "2026-09-21", Items: 3},
		{Day: "2026-09-20", Err: boom}, // 日曜
		{Day: "2026-09-19", Err: boom}, // 土曜
	}}
	if err := weekend.FailureError(); err != nil {
		t.Errorf("土日だけの失敗の FailureError = %v, want nil", err)
	}
	weekday := SyncResult{Failed: 2, Days: []SyncDay{
		{Day: "2026-09-20", Err: boom},
		{Day: "2026-09-18", Err: boom}, // 金曜
	}}
	err := weekday.FailureError()
	if err == nil || !strings.Contains(err.Error(), "2026-09-18") || strings.Contains(err.Error(), "2026-09-20") ||
		!strings.HasPrefix(err.Error(), "1 日") {
		t.Errorf("FailureError = %v, want 9/18 だけの失敗", err)
	}
}
