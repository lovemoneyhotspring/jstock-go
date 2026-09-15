package usmarket

import (
	"testing"
	"time"
)

func TestExpectedSession(t *testing.T) {
	cases := []struct{ day, want, why string }{
		{"2026-09-15", "2026-09-14", "火曜は月曜の夜"},
		{"2026-09-14", "2026-09-11", "月曜は金曜の夜"},
		{"2026-09-08", "2026-09-04", "労働者の日（9/7）明け"},
		{"2026-04-06", "2026-04-02", "聖金曜日（4/3）と週末明け"},
		{"2026-07-06", "2026-07-02", "独立記念日が土曜 → 7/3 に振替"},
		{"2026-11-27", "2026-11-25", "感謝祭（11/26）明け"},
		{"2027-01-04", "2026-12-31", "元日（金曜）と週末明け"},
		{"2022-01-03", "2021-12-31", "元日が土曜なら前年の 12/31 は休まない"},
		{"2026-06-22", "2026-06-18", "ジューンティーンス（6/19 金曜）と週末明け"},
		{"2026-12-28", "2026-12-24", "クリスマス（12/25 金曜）と週末明け"},
	}
	for _, c := range cases {
		if got := ExpectedSession(day(c.day)).Format(dateLayout); got != c.want {
			t.Errorf("%s（%s）: %s, want %s", c.day, c.why, got, c.want)
		}
	}
}

func TestEaster(t *testing.T) {
	for year, want := range map[int]string{2024: "2024-03-31", 2025: "2025-04-20", 2026: "2026-04-05", 2027: "2027-03-28"} {
		if got := easter(year).Format(dateLayout); got != want {
			t.Errorf("%d: %s, want %s", year, got, want)
		}
	}
}

func TestIsFresh(t *testing.T) {
	if IsFresh(nil, day("2026-09-15")) {
		t.Error("nil を新しいとした")
	}
	// 2026-09-15 の朝に 9/11 の値しか無い（FRED が 9/14 をまだ出していない）
	if IsFresh(&Session{Date: day("2026-09-11")}, day("2026-09-15")) {
		t.Error("前々夜の値を新しいとした")
	}
	if !IsFresh(&Session{Date: day("2026-09-14")}, day("2026-09-15")) {
		t.Error("前夜の値を古いとした")
	}
	// 判定日が JST の 0 時でも年月日で比べる
	jst := time.FixedZone("JST", 9*3600)
	if !IsFresh(&Session{Date: day("2026-09-14")}, time.Date(2026, 9, 15, 0, 0, 0, 0, jst)) {
		t.Error("判定日の時間帯で結果が変わった")
	}
}
