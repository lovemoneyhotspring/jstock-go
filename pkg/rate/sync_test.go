package rate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

type stubNews struct {
	byDay map[string][]broker.NewsItem
	fail  map[string]bool
	calls []string
}

func (s *stubNews) News(day string) ([]broker.NewsItem, error) {
	s.calls = append(s.calls, day)
	if s.fail[day] {
		return nil, errors.New("電文の失敗")
	}
	return s.byDay[day], nil
}

func setNow(t *testing.T, at time.Time) {
	t.Helper()
	clock.Now = func() time.Time { return at }
	t.Cleanup(func() { clock.Now = time.Now })
}

func eventCount(t *testing.T, store *Store, day string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(`SELECT event_count FROM news_days WHERE feed_date = ?`, day).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 07:10 の配信より前に回した日は event_count=0 で「済み」になる。直近 recent 日は
// 済みでも取り直すので、配信の後に回せばその日の動きが入る。
func TestSyncNewsRefetchesDayDeliveredAfterEarlyRun(t *testing.T) {
	ctx := context.Background()
	rating := []broker.NewsItem{{ID: "r1", Time: "0710", Genres: []string{broker.GenreRatingUp}, Codes: []string{"7203"},
		Headline: "<AI市況>【株価レーティング】引き上げ（9/14）：トヨタ"}}

	for _, c := range []struct {
		name   string
		recent int
		want   int
	}{
		{"recent=3 で取り直す", 3, 1},
		{"recent=0（旧挙動）は取り直さない", 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := openRateStore(t)
			src := &stubNews{byDay: map[string][]broker.NewsItem{}}

			// 06:00 JST: 当日のまとめはまだ配信されていない
			setNow(t, time.Date(2026, 9, 15, 6, 0, 0, 0, clock.Tokyo))
			if _, err := SyncNews(ctx, store, src, 5, c.recent, false); err != nil {
				t.Fatal(err)
			}
			if got := eventCount(t, store, "2026-09-15"); got != 0 {
				t.Fatalf("配信前 = %d", got)
			}

			// 07:30 JST: 配信された
			src.byDay["20260915"] = rating
			src.calls = nil
			setNow(t, time.Date(2026, 9, 15, 7, 30, 0, 0, clock.Tokyo))
			res, err := SyncNews(ctx, store, src, 5, c.recent, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := eventCount(t, store, "2026-09-15"); got != c.want {
				t.Fatalf("配信後の event_count = %d, want %d", got, c.want)
			}
			if res.Skipped != 5-c.recent || len(src.calls) != c.recent {
				t.Errorf("res = %+v calls = %v", res, src.calls)
			}
		})
	}
}

func TestSyncNewsSkipsDoneAndContinuesPastFailure(t *testing.T) {
	ctx := context.Background()
	store := openRateStore(t)
	setNow(t, time.Date(2026, 9, 15, 23, 30, 0, 0, clock.Tokyo)) // UTC ではもう 9/15 14:30
	if err := store.SaveEvents(ctx, "2026-09-12", nil, 10); err != nil {
		t.Fatal(err)
	}
	src := &stubNews{fail: map[string]bool{"20260913": true}}
	res, err := SyncNews(ctx, store, src, 5, 1, false)
	if err != nil {
		t.Fatalf("1 日の取得失敗で止まった: %v", err)
	}
	// 9/15 から遡る。9/13 は失敗しても先へ進み、9/12 は済みで飛ばし、9/11 まで届く
	if !reflect.DeepEqual(src.calls, []string{"20260915", "20260914", "20260913", "20260911"}) {
		t.Errorf("calls = %v", src.calls)
	}
	if res.Imported != 3 || res.Skipped != 1 || res.Failed != 1 {
		t.Errorf("res = %+v", res)
	}
	// 失敗した日は済みにならず、次の回でまた取りに行く
	src.calls, src.fail = nil, nil
	res, err = SyncNews(ctx, store, src, 5, 0, false)
	if err != nil || res.Failed != 0 {
		t.Fatalf("2 回目: res = %+v err = %v", res, err)
	}
	if !reflect.DeepEqual(src.calls, []string{"20260913"}) {
		t.Errorf("2 回目の calls = %v（失敗した 9/13 だけを取るはず。9/11 は 1 回目で済み）", src.calls)
	}
	// force なら済みの日も取る
	src.calls = nil
	if _, err := SyncNews(ctx, store, src, 5, 0, true); err != nil || len(src.calls) != 5 {
		t.Errorf("force: calls = %v err = %v", src.calls, err)
	}
}
