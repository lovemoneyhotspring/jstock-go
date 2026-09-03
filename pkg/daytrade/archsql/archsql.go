// Package archsql は J-Quants のアーカイブ（月ごとの Parquet）を DuckDB で読むための下ごしらえ。
//
// なぜ DuckDB か: Python 版は polars の式で母集団を絞り、同じ式を前夜の plan（1 日）と
// backtest（10 年のパネル）で共用していた。Go には polars に当たる表計算が無いので、
// 「同じ条件を 1 日と 10 年の両方に当てる」という設計は SQL で引き継ぐ——DuckDB は
// Parquet を直接クエリでき、窓関数（lag / rolling / rank）も揃っている。
// 集計は SQL、判断（ゲート・順位付け・株数）は Go の純粋関数、という分担にする。
package archsql

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// DateLayout はアーカイブの日付表記。
const DateLayout = "2006-01-02"

// Open はインメモリの DuckDB を開く。
func Open() (*sql.DB, error) { return storage.OpenDuckDB() }

// Source は端点の Parquet を read_parquet(...) の式にする。
// 期間に月ファイルが 1 つも無ければ ok=false（呼び出し側が空として扱う）。
func Source(arch *archive.Archive, ep archive.Endpoint, start, end time.Time) (expr string, ok bool) {
	months := arch.Months(ep)
	quoted := make([]string, 0, len(months))
	for _, month := range months {
		if !monthInRange(month, start, end) {
			continue
		}
		quoted = append(quoted, "'"+strings.ReplaceAll(arch.PathFor(ep, month), "'", "''")+"'")
	}
	if len(quoted) == 0 {
		return "", false
	}
	// union_by_name は列が後から増えても古い月と一緒に読むため（履歴の読み出しと同じ理由）。
	return "read_parquet([" + strings.Join(quoted, ", ") + "], union_by_name=true)", true
}

// monthInRange は "YYYY-MM" が期間に掛かるか。ゼロ値の端は無指定。
func monthInRange(month string, start, end time.Time) bool {
	first, err := time.Parse("2006-01", month)
	if err != nil {
		return false
	}
	last := first.AddDate(0, 1, -1)
	if !start.IsZero() && last.Before(truncate(start)) {
		return false
	}
	if !end.IsZero() && first.After(truncate(end)) {
		return false
	}
	return true
}

func truncate(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// Lit は日付を SQL のリテラルにする。
func Lit(day time.Time) string { return "DATE '" + day.Format(DateLayout) + "'" }

// MissingError は期間にアーカイブが無いときのエラー。
func MissingError(ep archive.Endpoint) error {
	return fmt.Errorf("%s のアーカイブがありません。jquants backfill / sync を先に", ep.Name())
}
