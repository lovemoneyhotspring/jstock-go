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
