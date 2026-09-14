package clock

import (
	"testing"
	"time"
)

func TestClock(t *testing.T) {
	tokyo := MustZone("Asia/Tokyo")
	refTime := time.Date(2026, 8, 29, 6, 20, 0, 0, time.UTC)

	formattedUTC := Fmt(refTime, time.UTC, false)
	if formattedUTC != "2026-08-29 06:20 UTC" {
		t.Fatalf("unexpected formatted UTC: %s", formattedUTC)
	}

	formattedJST := Fmt(refTime, tokyo, false)
	if formattedJST != "2026-08-29 15:20 JST" {
		t.Fatalf("unexpected formatted JST: %s", formattedJST)
	}

	formattedTime := FmtTime(refTime, tokyo)
	if formattedTime != "15:20 JST" {
		t.Fatalf("unexpected formatted time: %s", formattedTime)
	}

	iso := "2026-08-29T06:20:00+00:00"
	fmtIso := FmtISO(iso, tokyo)
	if fmtIso != "2026-08-29 15:20:00 JST" {
		t.Fatalf("unexpected FmtISO: %s", fmtIso)
	}

	dateOnly := "2026-08-29"
	if FmtISO(dateOnly, tokyo) != dateOnly {
		t.Fatalf("FmtISO should keep date-only string unchanged")
	}
}

// Now を差し替えると、時刻を返す関数がすべてそれに従う。
func TestNowInjection(t *testing.T) {
	fixed := time.Date(2026, 9, 14, 23, 30, 0, 0, time.UTC) // 9/15 08:30 JST
	Now = func() time.Time { return fixed }
	t.Cleanup(func() { Now = time.Now })

	if got := NowUTC(); !got.Equal(fixed) {
		t.Errorf("NowUTC = %v", got)
	}
	if got := NowJST(); got.Hour() != 8 || got.Day() != 15 {
		t.Errorf("NowJST = %v, want 2026-09-15 08:30 JST", got)
	}
	// 00:00〜09:00 JST は UTC の日付が 1 日早い。営業日の計算は JST の今日を使う
	if got := TodayUTC(); got.Format("2006-01-02") != "2026-09-14" {
		t.Errorf("TodayUTC = %v", got)
	}
	if got := TodayJST(); got.Format("2006-01-02") != "2026-09-15" || got.Location() != Tokyo {
		t.Errorf("TodayJST = %v (%v)", got, got.Location())
	}
	if got := StampISO(Tokyo); got != "2026-09-15T08:30:00.000000+09:00" {
		t.Errorf("StampISO = %s", got)
	}
}

func TestTokyoIsNeverUTC(t *testing.T) {
	if Tokyo == nil {
		t.Fatal("Tokyo が nil")
	}
	_, offset := time.Date(2026, 1, 1, 0, 0, 0, 0, Tokyo).Zone()
	if offset != 9*3600 {
		t.Errorf("Tokyo のオフセット = %d, want +09:00", offset)
	}
	// tzdata が無い環境の代替も +09:00
	if _, offset := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("JST", 9*3600)).Zone(); offset != 9*3600 {
		t.Errorf("代替の JST のオフセット = %d", offset)
	}
}

func TestParseDateJST(t *testing.T) {
	got, err := ParseDateJST("2026-09-15")
	if err != nil {
		t.Fatal(err)
	}
	if got.Location() != Tokyo || got.Hour() != 0 || got.Format("2006-01-02") != "2026-09-15" {
		t.Errorf("ParseDateJST = %v", got)
	}
	// JST の 0 時は UTC では前日 15 時。UTC の日付に丸めると 1 日ずれる
	if got.UTC().Day() != 14 {
		t.Errorf("UTC = %v", got.UTC())
	}
	if _, err := ParseDateJST("2026/09/15"); err == nil {
		t.Error("形式が違うのにエラーにならない")
	}
}
