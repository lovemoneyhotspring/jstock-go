package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// barsUnusable は、その銘柄の足で今回の判断をしてよいかを調べ、してはいけなければ理由を返す
// （してよければ空文字）。
//
// 古い足・読めない足のまま判断すると、戦略は「シグナル消滅」を出し、order_type = "market" なら
// 保有を全株成行で売る（2026-09-24 のレビュー W6）。その回はこの銘柄について売りも買いも出さない。
//
// あるべき最後の足は today（JST）の前の営業日。足は夜（19:05）に取り込むので、場中の run では
// 前営業日の足が最新になる。data check と違い 1 日の遅れも許さない（判断に使う足なので）。
// previousTradingDay は東証の営業日の前日を返す（カレンダーが無ければ平日で代用する。
// 祝日明けは古いとみなして止まる側に倒れる）。
func barsUnusable(bars []domain.Bar, readErr error, today time.Time,
	previousTradingDay func(time.Time) (time.Time, error),
) string {
	if readErr != nil {
		return fmt.Sprintf("足を読めない: %v", readErr)
	}
	if len(bars) == 0 {
		return "足が無い"
	}
	last := bars[len(bars)-1].Date
	lastDay, err := time.Parse("2006-01-02", last)
	if err != nil {
		return fmt.Sprintf("最後の足の日付 %q を読めない", last)
	}
	want, err := previousTradingDay(today)
	if err != nil {
		return fmt.Sprintf("前営業日を決められない: %v", err)
	}
	if lastDay.Before(want) {
		return fmt.Sprintf("足が古い（最終 %s、%s の足が無い）", last, want.Format("2006-01-02"))
	}
	return ""
}
