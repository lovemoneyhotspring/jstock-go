package regime

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// EPTopix は TOPIX の日足（寄り→引けのドリフトの材料）。
var EPTopix = archive.MustEndpoint("indices_bars_daily_topix")

// EPOptions225 は日経 225 オプションの日足（IV の材料）。
var EPOptions225 = archive.MustEndpoint("derivatives_bars_daily_options_225")

// TopixDrift は TOPIX の寄り→引けリターンの days 日平均（asOf まで）。
// 足が足りなければ nil（そのゲートは効かせない）。
func TopixDrift(arch *archive.Archive, asOf time.Time, days int) (*float64, error) {
	if days <= 0 {
		return nil, nil
	}
	// plan は前営業日を asOf に呼ぶ。asOf を含む直近 days 日の平均で、
	// 判断の時点で確定している値だけを使う。
	return topixMean(arch, asOf, days)
}

// topixMean は asOf までの直近 days 日の寄り→引けリターンの平均。
func topixMean(arch *archive.Archive, asOf time.Time, days int) (*float64, error) {
	start := asOf.AddDate(0, 0, -(days*2 + 20))
	source, ok := archsql.Source(arch, EPTopix, start, asOf)
	if !ok {
		return nil, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := fmt.Sprintf(`
WITH r AS (
  SELECT "Date" AS d, TRY_CAST("C" AS DOUBLE) / TRY_CAST("O" AS DOUBLE) - 1 AS ret
  FROM %s WHERE "Date" <= %s
)
SELECT avg(ret), count(ret) FROM (
  SELECT ret FROM r WHERE ret IS NOT NULL ORDER BY d DESC LIMIT %d
)`, source, archsql.Lit(asOf), days)
	var mean sql.NullFloat64
	var n int
	if err := db.QueryRow(query).Scan(&mean, &n); err != nil {
		return nil, fmt.Errorf("TOPIX のドリフト計算に失敗しました: %w", err)
	}
	if !mean.Valid || n < days {
		return nil, nil
	}
	v := mean.Float64
	return &v, nil
}

// DriftPoint は日付ごとのドリフト（前日までの平均）。
type DriftPoint struct {
	Date  time.Time
	Drift *float64
}

// TopixDriftSeries はバックテスト用: 日付ごとの drift（**前日まで**の平均）。
// 当日の寄り→引けを含めると、判断の時点で知り得ない値を使うことになる。
func TopixDriftSeries(arch *archive.Archive, start, end time.Time, days int) ([]DriftPoint, error) {
	if days <= 0 {
		return nil, nil
	}
	lookback := start.AddDate(0, 0, -(days*2 + 20))
	source, ok := archsql.Source(arch, EPTopix, lookback, end)
	if !ok {
		return nil, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := fmt.Sprintf(`
WITH r AS (
  SELECT "Date" AS d, TRY_CAST("C" AS DOUBLE) / TRY_CAST("O" AS DOUBLE) - 1 AS ret
  FROM %s WHERE "Date" <= %s
)
SELECT d, lag(avg_ret) OVER (ORDER BY d) AS drift FROM (
  SELECT d, avg(ret) OVER (ORDER BY d ROWS BETWEEN %d PRECEDING AND CURRENT ROW) AS avg_ret,
         count(ret) OVER (ORDER BY d ROWS BETWEEN %d PRECEDING AND CURRENT ROW) AS n
  FROM r
) WHERE n = %d ORDER BY d`, source, archsql.Lit(end), days-1, days-1, days)
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("TOPIX のドリフト系列の計算に失敗しました: %w", err)
	}
	defer rows.Close()
	var out []DriftPoint
	for rows.Next() {
		var d time.Time
		var drift sql.NullFloat64
		if err := rows.Scan(&d, &drift); err != nil {
			return nil, err
		}
		p := DriftPoint{Date: d.UTC()}
		if drift.Valid {
			v := drift.Float64
			p.Drift = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// IVOn はその日の日経 225 オプション BaseVol の中央値（無ければ nil）。
func IVOn(arch *archive.Archive, day time.Time) (*float64, error) {
	source, ok := archsql.Source(arch, EPOptions225, day, day)
	if !ok {
		return nil, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := fmt.Sprintf(`SELECT median(TRY_CAST("BaseVol" AS DOUBLE)) FROM %s WHERE "Date" = %s`,
		source, archsql.Lit(day))
	var iv sql.NullFloat64
	if err := db.QueryRow(query).Scan(&iv); err != nil {
		// BaseVol 列が無い古いアーカイブでもゲートを止めない（診断値が欠けるだけ）
		return nil, nil
	}
	if !iv.Valid {
		return nil, nil
	}
	v := iv.Float64
	return &v, nil
}

// IVPoint は日付ごとの前日 IV。
type IVPoint struct {
	Date   time.Time
	IVPrev *float64
}

// IVByDay は日経 225 オプションの BaseVol の日次中央値（**前日値**を IVPrev に）。
func IVByDay(arch *archive.Archive, start, end time.Time) ([]IVPoint, error) {
	source, ok := archsql.Source(arch, EPOptions225, start.AddDate(0, 0, -10), end)
	if !ok {
		return nil, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := fmt.Sprintf(`
SELECT d, lag(iv) OVER (ORDER BY d) AS iv_prev FROM (
  SELECT "Date" AS d, median(TRY_CAST("BaseVol" AS DOUBLE)) AS iv FROM %s GROUP BY "Date"
) ORDER BY d`, source)
	rows, err := db.Query(query)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []IVPoint
	for rows.Next() {
		var d time.Time
		var iv sql.NullFloat64
		if err := rows.Scan(&d, &iv); err != nil {
			return nil, err
		}
		p := IVPoint{Date: d.UTC()}
		if iv.Valid {
			v := iv.Float64
			p.IVPrev = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
