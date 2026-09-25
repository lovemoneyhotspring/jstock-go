package main

import (
	"testing"
	"time"
)

func TestTodayAt(t *testing.T) {
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	got, ok, err := todayAt(day, "08:59:51.2")
	if err != nil || !ok {
		t.Fatalf("読めない: %v %v", ok, err)
	}
	if want := time.Date(2026, 9, 24, 8, 59, 51, 200_000_000, jst); !got.Equal(want) {
		t.Errorf("= %v, want %v", got, want)
	}
	if got, ok, _ := todayAt(day, "08:59:52"); !ok || got.Second() != 52 {
		t.Errorf("小数なし = %v %v", got, ok)
	}
	if _, ok, err := todayAt(day, ""); ok || err != nil {
		t.Errorf("空は ok=false・誤りなし: %v %v", ok, err)
	}
	for _, bad := range []string{"8:59", "085951", "08:59:61"} {
		if _, _, err := todayAt(day, bad); err == nil {
			t.Errorf("%q を通した", bad)
		}
	}
}
