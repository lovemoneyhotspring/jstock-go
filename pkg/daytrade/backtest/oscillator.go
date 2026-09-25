package backtest

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// oscLookbackDays は期間の頭の行にも OscBars 本の足をそろえるための暦日（universe の読み込みと同じ）。
const oscLookbackDays = 200

// attachOscillators はパネルの各行に、前日の引けまでの RSI(2) とストキャス RSI(14,14) を付ける
// （Row.RSI2 / StochRSI14）。前夜の plan（universe.loadOscillators）と同じ足の条件・同じ本数で計算する。
//
// パネルは売買代金の下限で行を削っていて足が歯抜けになるので、足は別に読む。銘柄ごとに日付順で
// 流し、パネルの行の日付に来たらその前の足までで計算する（10 年ぶんの終値を抱え込まない）。
func attachOscillators(db *sql.DB, arch *archive.Archive, panel *Panel, start, end time.Time) error {
	src, ok := archsql.Source(arch, universe.EPBars, start.AddDate(0, 0, -oscLookbackDays), end)
	if !ok {
		return archsql.MissingError(universe.EPBars)
	}
	byCode := map[string][]int{} // パネルは日付順なので、銘柄ごとの行番号も日付順
	for i, r := range panel.Rows {
		byCode[r.Code] = append(byCode[r.Code], i)
	}
	rows, err := db.Query(fmt.Sprintf(`
SELECT code, d, c / exp(sum(ln(af)) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS z
FROM (
  SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code, TRY_CAST("C" AS DOUBLE) AS c,
         coalesce(nullif(TRY_CAST("AdjFactor" AS DOUBLE), 0), 1) AS af
  FROM %s
  WHERE "Date" <= %s AND TRY_CAST("O" AS DOUBLE) > 0 AND TRY_CAST("C" AS DOUBLE) > 0
)
ORDER BY code, d`, src, archsql.Lit(end)))
	if err != nil {
		return fmt.Errorf("オシレータの足の読み込みに失敗しました: %w", err)
	}
	defer rows.Close()
	var (
		code string
		z    []float64
		next []int // この銘柄のまだ付けていないパネルの行
	)
	for rows.Next() {
		var c string
		var d time.Time
		var v float64
		if err := rows.Scan(&c, &d, &v); err != nil {
			return err
		}
		if c != code {
			code, z, next = c, z[:0], byCode[c]
		}
		// この足より前の日付の行（足が無い日の行は無いはずだが、念のため飛ばす）
		for len(next) > 0 && panel.Rows[next[0]].Date.Before(d) {
			next = next[1:]
		}
		if len(next) > 0 && sameDate(panel.Rows[next[0]].Date, d) {
			r := &panel.Rows[next[0]]
			r.RSI2, r.StochRSI14 = universe.Oscillators(z)
			next = next[1:]
		}
		z = append(z, v)
		if len(z) > 2*universe.OscBars {
			z = append(z[:0], z[len(z)-universe.OscBars:]...)
		}
	}
	return rows.Err()
}

func sameDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
