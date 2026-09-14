package rate

import (
	"context"
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// NewsSource はニュースを日付（YYYYMMDD）で引く口。立花証券のブローカーが満たす。
type NewsSource interface {
	News(day string) ([]broker.NewsItem, error)
}

// SyncNewsDay は取り込んだ 1 日ぶん。
type SyncNewsDay struct {
	Day    string
	News   int
	Events int
}

// SyncNewsResult は SyncNews の結果。
type SyncNewsResult struct {
	Imported int
	Skipped  int
	Events   int
	Days     []SyncNewsDay
}

// SyncNews は JST の今日から days 日さかのぼって、ニュースからレーティングの動きを取り込む。
//
// 取り込み済みの日は飛ばすが、**直近 recent 日は済みでも取り直す**。レーティングの
// まとめは前営業日ぶんが 07:10 に配信されるので、それより前に回すとその日は
// event_count=0 で「済み」になり、以後二度と取りに行かなくなる。force なら全部取り直す。
// 1 日でも取得に失敗したら止める（済んだ日は記録簿に残る）。
func SyncNews(ctx context.Context, store *Store, source NewsSource, days, recent int, force bool) (SyncNewsResult, error) {
	var res SyncNewsResult
	done, err := store.ImportedDays(ctx)
	if err != nil {
		return res, err
	}
	now := clock.NowJST()
	for i := 0; i < days; i++ {
		day := now.AddDate(0, 0, -i)
		key := day.Format("2006-01-02")
		if done[key] && !force && i >= recent {
			res.Skipped++
			continue
		}
		items, err := source.News(day.Format("20060102"))
		if err != nil {
			return res, fmt.Errorf("%s のニュース取得に失敗: %w", key, err)
		}
		events := EventsFromNews(day, items)
		if err := store.SaveEvents(ctx, key, events, len(items)); err != nil {
			return res, err
		}
		res.Imported++
		res.Events += len(events)
		res.Days = append(res.Days, SyncNewsDay{Day: key, News: len(items), Events: len(events)})
	}
	return res, nil
}
