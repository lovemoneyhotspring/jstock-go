package rate

import (
	"context"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// NewsSource はニュースを日付（YYYYMMDD）で引く口。立花証券のブローカーが満たす（news.Source と同じ）。
type NewsSource = news.Source

// SyncNewsDay は取り込んだ 1 日ぶん。
type SyncNewsDay struct {
	Day    string
	News   int
	Events int
	// Err は取れなかったとき。済みにしないので、次回また取りに行く。
	Err error
}

// SyncNewsResult は SyncNews の結果。
type SyncNewsResult struct {
	Imported int
	Skipped  int
	Failed   int
	Events   int
	Days     []SyncNewsDay
}

// SyncNews は JST の今日から days 日さかのぼって、ニュースからレーティングの動きを取り込む。
//
// 取り込み済みの日は飛ばすが、**直近 recent 日は済みでも取り直す**。レーティングの
// まとめは前営業日ぶんが 07:10 に配信されるので、それより前に回すとその日は
// event_count=0 で「済み」になり、以後二度と取りに行かなくなる。force なら全部取り直す。
// 1 日取れなくても止めない（news.Sync と同じ）。その日は済みにしないので次回また取る。
// 日曜などに、立花の応答が空（aCLMMfdsNews が無い）で返る日がある。そこで止めると、それより前の日が
// 永遠に届かない。記録簿への書き込みに失敗したら止める。
func SyncNews(ctx context.Context, store *Store, source NewsSource, days, recent int, force bool) (SyncNewsResult, error) {
	var res SyncNewsResult
	done, err := store.ImportedDays(ctx)
	if err != nil {
		return res, err
	}
	// 日のめぐり方（済みの飛ばし・直近の取り直し・1 日の失敗で止めない）は news.Sync と共通（news.Walk）
	res.Skipped, err = news.Walk(clock.NowJST(), source, days, recent, force, done,
		func(day time.Time, key string, items []broker.NewsItem, fetchErr error) error {
			if fetchErr != nil {
				res.Failed++
				res.Days = append(res.Days, SyncNewsDay{Day: key, Err: fmt.Errorf("%s のニュース取得に失敗: %w", key, fetchErr)})
				return nil
			}
			events := EventsFromNews(day, items)
			if err := store.SaveEvents(ctx, key, events, len(items)); err != nil {
				return err
			}
			res.Imported++
			res.Events += len(events)
			res.Days = append(res.Days, SyncNewsDay{Day: key, News: len(items), Events: len(events)})
			return nil
		})
	return res, err
}
