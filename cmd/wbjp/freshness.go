package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// tradingDayGate は今日 run してよいかを決める。休場日なら理由を返す（発注せずに正常終了）。
//
// 休場日（平日の祝日を含む）に発注する回を回すと、前営業日の足で判断した注文が翌営業日に
// 回り、当日限りの前提（placed_on・差金決済の柵）が崩れる。カレンダーが読めないと祝日が
// 分からないので、発注する回は止める（dry-run は平日として続ける）。
func tradingDayGate(cal *calendar.Calendar, today time.Time, canLive bool) (skip string, err error) {
	if cal.Empty() {
		if canLive {
			return "", fmt.Errorf("取引カレンダーが読めないため発注を中止しました（祝日に発注しないため。jquants sync で取り込む）")
		}
		return "", nil
	}
	if !cal.IsTradingDay(today) {
		return fmt.Sprintf("%s は休場日のため判断も発注もしません", today.Format("2006-01-02")), nil
	}
	return "", nil
}

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

// reportUnusableBars は足が古い・読めない銘柄をログとダイジェストに残す。保有中の銘柄が
// あれば Discord にも通知する（その銘柄は損切り・利確・時間切れも止まる。足の取り込みが
// 止まったまま気づかないと、下げても売らない）。通知した保有銘柄の行を返す（テスト用）。
func reportUnusableBars(unusable map[string]string, positions map[string]domain.Position, logger *logging.Logger) []string {
	if len(unusable) == 0 {
		return nil
	}
	lines := make([]string, 0, len(unusable))
	var held []string
	for sym, why := range unusable {
		if pos, ok := positions[sym]; ok && pos.Quantity.IsPositive() {
			line := fmt.Sprintf("%s（保有 %s 株）: %s", sym, pos.Quantity, why)
			lines = append(lines, line)
			held = append(held, line)
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", sym, why))
	}
	sort.Strings(lines)
	sort.Strings(held)
	logger.Warn("wbjp.bars_unusable", "足が古い・読めないため、この回は判断しません（売りも買いも出さない）:\n"+strings.Join(lines, "\n"))
	digest.Anomaly("wbjp.bars_unusable", fmt.Sprintf("%d 銘柄の足が古い・読めない（その銘柄は判断しない。保有 %d 銘柄は損切りも止まる）",
		len(unusable), len(held)))
	if len(held) > 0 {
		run.Alert(fmt.Sprintf("wbjp: 足が古い保有銘柄 %d 件は損切りも止まっています（足の取り込みを確かめてください）", len(held)),
			strings.Join(held, "\n"))
	}
	return held
}
