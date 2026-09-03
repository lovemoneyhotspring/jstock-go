package risk

import (
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestTradingDaysHeld(t *testing.T) {
	cases := []struct {
		name    string
		created string
		asOf    string
		want    int
	}{
		// 2026-09-03 は木曜、09-04 金曜、09-05 土曜、09-06 日曜、09-07 月曜。
		{"同日は0", "2026-09-03", "2026-09-03", 0},
		{"翌営業日は1", "2026-09-03", "2026-09-04", 1},
		{"土曜は数えない", "2026-09-03", "2026-09-05", 1},
		{"日曜も数えない", "2026-09-03", "2026-09-06", 1},
		{"週明けは2", "2026-09-03", "2026-09-07", 2},
		{"1週間で5営業日", "2026-09-03", "2026-09-10", 5},
		{"過去日付は0", "2026-09-03", "2026-09-01", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tradingDaysHeld(day(tc.created), day(tc.asOf))
			if got != tc.want {
				t.Errorf("tradingDaysHeld(%s → %s) = %d, want %d", tc.created, tc.asOf, got, tc.want)
			}
		})
	}
}

func TestTradingDaysHeldIgnoresWeekendGap(t *testing.T) {
	// 暦日で数えると 4 日だが、営業日では 2 日。連休を挟んで
	// 時間切れ手仕舞いが早まらないことを確かめる。
	calendar := int(day("2026-09-07").Sub(day("2026-09-03")).Hours() / 24)
	if calendar != 4 {
		t.Fatalf("前提が崩れている: 暦日 %d", calendar)
	}
	if got := tradingDaysHeld(day("2026-09-03"), day("2026-09-07")); got != 2 {
		t.Errorf("営業日は 2 日であるべき: %d", got)
	}
}
