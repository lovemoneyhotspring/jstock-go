package news

import (
	"context"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// Source はニュースを日付（YYYYMMDD）で引く口。立花証券のブローカーが満たす。
type Source interface {
	News(day string) ([]broker.NewsItem, error)
}

// SyncDay は取り込もうとした 1 日ぶん。
type SyncDay struct {
	Day   string
	Items int
	New   int
	// Err は取れなかったとき。台帳に失敗として残り、次回また取りに行く。
	Err error
}

// SyncResult は Sync の結果。
type SyncResult struct {
	Imported int
	Skipped  int
	Failed   int
	Added    int
	Days     []SyncDay
}

// Sync は JST の今日から days 日さかのぼってニュースを取り込む。
//
//   - 済みの日（DoneDays）は飛ばす。ただし直近 recent 日は訂正・追記が入るので取り直す。
//     force なら全部取り直す
//   - 1 日取れなくても止めない。その日は台帳に失敗として残り、済みにならないので次回また取る
//   - 記録簿への書き込みに失敗したら止める
func Sync(ctx context.Context, store *Store, source Source, days, recent int, force bool) (SyncResult, error) {
	var res SyncResult
	done, err := store.DoneDays(ctx)
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
			if rerr := store.RecordFailure(ctx, key, now, err.Error()); rerr != nil {
				return res, rerr
			}
			res.Failed++
			res.Days = append(res.Days, SyncDay{Day: key, Err: err})
			continue
		}
		n, err := store.Save(ctx, key, items, now)
		if err != nil {
			return res, err
		}
		res.Imported++
		res.Added += n
		res.Days = append(res.Days, SyncDay{Day: key, Items: len(items), New: n})
	}
	return res, nil
}
