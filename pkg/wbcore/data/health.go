package data

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// 足の蓄積が健全かを調べる。cron が黙って止まっているのを見つけるため。
//
// 日足の「最後にいつ取れたか」を機械的に見る。止まっていることに気づくのが
// 遅れるほど、判断は古い足のまま続く。

// DailyStaleAfterDays は日足がこの日数より古ければ「止まっている」とみなす閾値。
// 週末（2 日）＋祝日の余裕を見て 4 日。
const DailyStaleAfterDays = 4

// Coverage は 1 銘柄の蓄積状況。
type Coverage struct {
	Symbol string
	Bars   int
	// First / Last は YYYY-MM-DD。足が無ければ空文字。
	First string
	Last  string
	// Stale は最後の足が、あるべき日より古いこと。
	Stale bool
}

// Healthy は足があり、かつ止まっていないこと。
func (c Coverage) Healthy() bool { return c.Bars > 0 && !c.Stale }

// Describe は状況の 1 行説明。
func (c Coverage) Describe() string {
	if c.Bars == 0 {
		return "足が無い"
	}
	if c.Stale {
		return fmt.Sprintf("最終 %s で止まっている", c.Last)
	}
	return "正常"
}

// Check は銘柄ごとの蓄積状況を返す。today がゼロ値なら UTC の今日。
func Check(root string, symbols []string, today time.Time) ([]Coverage, error) {
	if today.IsZero() {
		today = clock.TodayUTC()
	}
	store := NewBarStore(root)
	out := make([]Coverage, 0, len(symbols))
	for _, symbol := range symbols {
		bars, err := store.Read(symbol, "", "")
		if err != nil {
			return nil, err
		}
		if len(bars) == 0 {
			out = append(out, Coverage{Symbol: symbol})
			continue
		}
		first, last := bars[0].Date, bars[len(bars)-1].Date
		out = append(out, Coverage{Symbol: symbol, Bars: len(bars), First: first, Last: last, Stale: isStale(last, today)})
	}
	return out, nil
}

func isStale(last string, today time.Time) bool {
	lastDay, err := time.ParseInLocation("2006-01-02", last, time.UTC)
	if err != nil {
		// 日付として読めない足は「いつのものか分からない」＝当てにできない
		return true
	}
	todayUTC := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	days := int(todayUTC.Sub(lastDay).Hours() / 24)
	return days > DailyStaleAfterDays
}
