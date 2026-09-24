package news

import (
	"context"
	"fmt"
	"strings"
	"time"

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

// FailureError は営業日の日単位の失敗があればエラーを返す（news sync の終了コードに使う）。
// 失敗した日は台帳に残り次回また取りに行くが、終了 0 だと cron のログを読まない限り
// 取り込みが止まっていることに気づけない。
//
// 休場日（closed。nil なら土日）の失敗は数えない（ClosedFunc。記事の無い日曜は毎週失敗になり、
// 朝の --days 5 が月〜木に毎回 1 で終わってしまう）。失敗の件数と表示は Failed・Days に残る。
func (r SyncResult) FailureError(closed ClosedFunc) error {
	outcomes := make([]DayOutcome, len(r.Days))
	for i, d := range r.Days {
		outcomes[i] = DayOutcome{Day: d.Day, Err: d.Err}
	}
	return FailedDaysError(outcomes, closed)
}

// DayOutcome は日単位の取り込みの結果（Day は JST の "2006-01-02"）。FailedDaysError に渡す。
type DayOutcome struct {
	Day string
	Err error
}

// FailedDaysError は営業日の失敗があればエラーを返す。news sync と rate sync が同じ規則で
// 終了コードを決めるための共通の判定（片方だけ直して判定が割れないように 1 か所に置く）。
// 休場日（closed。nil なら土日）の失敗は数えない。
func FailedDaysError(days []DayOutcome, closed ClosedFunc) error {
	closed = closed.orWeekend()
	var failed []string
	for _, d := range days {
		if d.Err == nil {
			continue
		}
		if day, err := time.ParseInLocation("2006-01-02", d.Day, clock.Tokyo); err == nil && closed(day) {
			continue
		}
		failed = append(failed, d.Day)
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("%d 日の取り込みに失敗しました（%s。次回の sync で取り直します）", len(failed), strings.Join(failed, ", "))
}

// Walk は JST の now から days 日さかのぼり、取るべき日のニュースを source から引いて visit に渡す。
// news sync と rate sync（レーティングの動き）が同じ電文を同じ規則で引くための共通の骨組み。
//
//   - done（済みの日）は飛ばす。ただし直近 recent 日は訂正・追記が入るので取り直す。force なら全部取り直す
//   - 1 日取れなくても止めない。取れなかった日は fetchErr 付きで visit に渡す（済みにするかは visit が決める）
//   - visit がエラーを返したら（記録簿への書き込みの失敗など）そこで止める
//
// 戻りは飛ばした日数。
func Walk(now time.Time, source Source, days, recent int, force bool, done map[string]bool,
	visit func(day time.Time, key string, items []broker.NewsItem, fetchErr error) error) (skipped int, err error) {
	now = now.In(clock.Tokyo)
	for i := 0; i < days; i++ {
		day := now.AddDate(0, 0, -i)
		key := day.Format("2006-01-02")
		if done[key] && !force && i >= recent {
			skipped++
			continue
		}
		items, ferr := source.News(day.Format("20060102"))
		if err := visit(day, key, items, ferr); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
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
	res.Skipped, err = Walk(now, source, days, recent, force, done,
		func(_ time.Time, key string, items []broker.NewsItem, fetchErr error) error {
			if fetchErr != nil {
				if err := store.RecordFailure(ctx, key, now, fetchErr.Error()); err != nil {
					return err
				}
				res.Failed++
				res.Days = append(res.Days, SyncDay{Day: key, Err: fetchErr})
				return nil
			}
			n, err := store.Save(ctx, key, items, now)
			if err != nil {
				return err
			}
			res.Imported++
			res.Added += n
			res.Days = append(res.Days, SyncDay{Day: key, Items: len(items), New: n})
			return nil
		})
	return res, err
}
