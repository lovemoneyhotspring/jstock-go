package calendar

import (
	"testing"
	"time"
)

func d(iso string) time.Time {
	t, _ := time.Parse("2006-01-02", iso)
	return t
}

func TestIsTradingDayWithinRange(t *testing.T) {
	// 2026-01-05(月) と 07(水) だけが営業日。06 は範囲内なので「休場」と答える
	cal := New([]time.Time{d("2026-01-05"), d("2026-01-07")})
	if !cal.IsTradingDay(d("2026-01-05")) {
		t.Error("営業日を休場としている")
	}
	if cal.IsTradingDay(d("2026-01-06")) {
		t.Error("カレンダーに無い範囲内の日を営業日としている")
	}
}

func TestIsTradingDayOutsideRangeFallsBackToWeekdays(t *testing.T) {
	// 範囲外で「休場」と答えると、まだ取り込んでいない先の日付を全部止めてしまう
	cal := New([]time.Time{d("2026-01-05")})
	if !cal.IsTradingDay(d("2026-06-01")) { // 月曜
		t.Error("範囲外の平日を営業日としていない")
	}
	if cal.IsTradingDay(d("2026-06-06")) { // 土曜
		t.Error("範囲外の土曜を営業日としている")
	}
}

func TestEmptyCalendarUsesWeekdays(t *testing.T) {
	cal := New(nil)
	if !cal.Empty() {
		t.Error("空でない")
	}
	if !cal.IsTradingDay(d("2026-09-03")) { // 木曜
		t.Error("平日を営業日としていない")
	}
	if cal.IsTradingDay(d("2026-09-05")) { // 土曜
		t.Error("土曜を営業日としている")
	}
}

func TestNextAndPreviousTradingDay(t *testing.T) {
	cal := New([]time.Time{d("2026-01-05"), d("2026-01-06"), d("2026-01-09")})
	next, err := cal.NextTradingDay(d("2026-01-06"), false)
	if err != nil || !next.Equal(d("2026-01-09")) {
		t.Errorf("NextTradingDay = %v, %v; want 2026-01-09", next, err)
	}
	// inclusive なら自分自身も候補
	next, _ = cal.NextTradingDay(d("2026-01-06"), true)
	if !next.Equal(d("2026-01-06")) {
		t.Errorf("inclusive の NextTradingDay = %v", next)
	}
	prev, err := cal.PreviousTradingDay(d("2026-01-09"))
	if err != nil || !prev.Equal(d("2026-01-06")) {
		t.Errorf("PreviousTradingDay = %v, %v; want 2026-01-06", prev, err)
	}
}

func TestPreviousTradingDays(t *testing.T) {
	cal := New([]time.Time{d("2026-01-05"), d("2026-01-06"), d("2026-01-07"), d("2026-01-08")})
	days, err := cal.PreviousTradingDays(d("2026-01-08"), 2)
	if err != nil || len(days) != 2 {
		t.Fatalf("PreviousTradingDays = %v, %v", days, err)
	}
	// 新しい順に返る（資産曲線ゲートは直近から数える）
	if !days[0].Equal(d("2026-01-07")) || !days[1].Equal(d("2026-01-06")) {
		t.Errorf("並びが違う: %v", days)
	}
}

func TestNewDeduplicatesAndSorts(t *testing.T) {
	cal := New([]time.Time{d("2026-01-07"), d("2026-01-05"), d("2026-01-07")})
	if len(cal.Days()) != 2 {
		t.Errorf("重複を除いていない: %v", cal.Days())
	}
	if !cal.Days()[0].Equal(d("2026-01-05")) {
		t.Errorf("昇順になっていない: %v", cal.Days())
	}
}
