package calendar

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
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

// Closed は news.ClosedFunc に渡す休場日の判定。JST の時刻（深夜 0 時台・朝）でも
// その日の暦で答える。範囲外・空のカレンダーは土日（IsTradingDay の代用と同じ）。
func TestClosed(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	// 2025-12-30(火) は営業日、12-31(水)〜2026-01-02(金) は休場、01-05(月) は営業日
	cal := New([]time.Time{d("2025-12-30"), d("2026-01-05")})
	cases := []struct {
		day  time.Time
		want bool
	}{
		{time.Date(2025, 12, 30, 0, 30, 0, 0, jst), false},
		{time.Date(2025, 12, 31, 8, 0, 0, 0, jst), true}, // 平日の休場日
		{time.Date(2026, 1, 2, 23, 59, 0, 0, jst), true},
		{time.Date(2026, 1, 5, 6, 0, 0, 0, jst), false},
		{time.Date(2026, 6, 1, 6, 0, 0, 0, jst), false}, // 範囲外の月曜
		{time.Date(2026, 6, 6, 6, 0, 0, 0, jst), true},  // 範囲外の土曜
	}
	for _, c := range cases {
		if got := cal.Closed(c.day); got != c.want {
			t.Errorf("Closed(%s) = %v, want %v", c.day.Format(time.RFC3339), got, c.want)
		}
	}
	empty := New(nil)
	if empty.Closed(time.Date(2025, 12, 31, 8, 0, 0, 0, jst)) || !empty.Closed(time.Date(2026, 1, 3, 8, 0, 0, 0, jst)) {
		t.Error("空のカレンダーが土日の判定に戻っていない")
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

func str(v string) *string { return &v }

// 半日立会（HolDiv = 2）は営業日に数えたまま、半日の印だけが付く。
// 前後の営業日の並び（ギャップの基準になる前営業日）は変えない。
func TestFromArchiveMarksHalfDays(t *testing.T) {
	arch := archive.NewArchive(t.TempDir())
	frame := &archive.Frame{Columns: []string{"Date", "HolDiv"}}
	for _, row := range [][2]string{
		{"2026-12-29", "1"},
		{"2026-12-30", "2"}, // 半日立会
		{"2026-12-31", "0"}, // 休場
		{"2027-01-04", "2"}, // 半日立会
		{"2027-01-05", "1"},
	} {
		frame.AppendRow(map[string]*string{"Date": str(row[0]), "HolDiv": str(row[1])})
	}
	if _, err := arch.Upsert(archive.CalendarEndpoint(), frame); err != nil {
		t.Fatal(err)
	}
	cal := FromArchive(arch)

	cases := []struct {
		day           string
		trading, half bool
	}{
		{"2026-12-29", true, false},
		{"2026-12-30", true, true},
		{"2026-12-31", false, false},
		{"2027-01-04", true, true},
		{"2027-01-05", true, false},
		{"2027-06-01", true, false}, // 範囲外の平日: 営業日で代用するが、半日とは答えない
	}
	for _, c := range cases {
		if got := cal.IsTradingDay(d(c.day)); got != c.trading {
			t.Errorf("%s: IsTradingDay = %v, want %v", c.day, got, c.trading)
		}
		if got := cal.IsHalfDay(d(c.day)); got != c.half {
			t.Errorf("%s: IsHalfDay = %v, want %v", c.day, got, c.half)
		}
	}
	// 半日立会の翌営業日から見た前営業日は、半日立会の日のまま
	if prev, err := cal.PreviousTradingDay(d("2027-01-05")); err != nil || !prev.Equal(d("2027-01-04")) {
		t.Errorf("PreviousTradingDay = %v, %v; want 2027-01-04", prev, err)
	}
}

// New で作ったカレンダー（半日の情報なし）・空のカレンダーは、どの日も半日と答えない。
func TestIsHalfDayWithoutHalfDayInfo(t *testing.T) {
	if New([]time.Time{d("2026-01-05")}).IsHalfDay(d("2026-01-05")) {
		t.Error("半日の情報が無いのに半日と答えた")
	}
	if New(nil).IsHalfDay(d("2026-01-05")) {
		t.Error("空のカレンダーが半日と答えた")
	}
	if FromArchive(archive.NewArchive(t.TempDir())).IsHalfDay(d("2026-01-05")) {
		t.Error("カレンダーの無いアーカイブが半日と答えた")
	}
}
