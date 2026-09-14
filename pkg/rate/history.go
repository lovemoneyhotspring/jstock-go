package rate

import (
	"context"
	"fmt"
	"time"
)

// MonthRange は YYYYMM の範囲 [from, to] を月ごとに並べる。
func MonthRange(from, to string) ([]string, error) {
	parse := func(s string) (time.Time, error) {
		t, err := time.Parse("200601", s)
		if err != nil || len(s) != 6 {
			return time.Time{}, fmt.Errorf("年月は YYYYMM で渡してください: %q", s)
		}
		return t, nil
	}
	start, err := parse(from)
	if err != nil {
		return nil, err
	}
	end, err := parse(to)
	if err != nil {
		return nil, err
	}
	if end.Before(start) {
		return nil, fmt.Errorf("終了 %s が開始 %s より前です", to, from)
	}
	var months []string
	for m := start; !m.After(end); m = m.AddDate(0, 1, 0) {
		months = append(months, m.Format("200601"))
	}
	return months, nil
}

// MonthFetcher は年月（YYYYMM）の一覧ページを取る。試験では差し替える。
type MonthFetcher func(ctx context.Context, yearMonth string) (string, error)

// TradersFetcher はトレーダーズの月別一覧を FetchWithRetry で取る。
func TradersFetcher(attempts int, backoff time.Duration) MonthFetcher {
	return func(ctx context.Context, yearMonth string) (string, error) {
		return FetchWithRetry(ctx, TradersURL+yearMonth, attempts, backoff)
	}
}

// MonthResult は ImportMonths の 1 か月ぶん。
type MonthResult struct {
	YearMonth string
	Rows      int
	Added     int
	// ParseErr は取れたが行を読めなかった（古い月は行が無いこともある）。
	// その月は取り込み済みにしないので、次回また取りに行く。
	ParseErr error
}

// ImportResult は ImportMonths の合計。
type ImportResult struct {
	Fetched int
	Skipped int
	Rows    int
}

// ImportMonths は月別一覧を順に取り込む。
//
//   - 取り込み済みの月は飛ばす（force で取り直す）
//   - 取得に失敗したら（FetchWithRetry で取り直しても駄目なら）そこで止める。
//     それまでに済んだ月は記録簿に残るので、再実行すれば続きから進む
//   - 行を読めない月は止めずに次へ（取り込み済みにはしない）
//   - 相手に負担をかけないよう、取りに行く間に interval 空ける
//
// each は月ごとに呼ばれる（nil 可）。
func ImportMonths(ctx context.Context, store *Store, months []string, force bool, interval time.Duration,
	fetch MonthFetcher, each func(MonthResult)) (ImportResult, error) {
	var res ImportResult
	done, err := store.ImportedMonths(ctx)
	if err != nil {
		return res, err
	}
	for _, ym := range months {
		if done[ym] && !force {
			res.Skipped++
			continue
		}
		if res.Fetched > 0 && interval > 0 {
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			case <-time.After(interval):
			}
		}
		html, err := fetch(ctx, ym)
		if err != nil {
			return res, fmt.Errorf("%s: %w", ym, err)
		}
		res.Fetched++
		entries, err := ParseTraders(html, ym)
		if err != nil {
			if each != nil {
				each(MonthResult{YearMonth: ym, ParseErr: err})
			}
			continue
		}
		added, err := store.SaveTraders(ctx, ym, entries)
		if err != nil {
			return res, err
		}
		res.Rows += len(entries)
		if each != nil {
			each(MonthResult{YearMonth: ym, Rows: len(entries), Added: added})
		}
	}
	return res, nil
}
