package config

import (
	"testing"
	"time"
)

// TestWaitsForUs は us_stale_wait_until より前の回だけが待つこと（ちょうどの時刻は待たない）。
func TestWaitsForUs(t *testing.T) {
	jst := time.FixedZone("JST", 9*3600)
	at := func(h, m int) time.Time { return time.Date(2026, 9, 15, h, m, 0, 0, jst).UTC() }
	r := Regime{UsStaleWaitUntil: "09:12"}
	for _, c := range []struct {
		h, m int
		want bool
	}{{9, 1, true}, {9, 10, true}, {9, 12, false}, {9, 13, false}} {
		if got := r.WaitsForUs(at(c.h, c.m), jst); got != c.want {
			t.Errorf("%02d:%02d: %v, want %v", c.h, c.m, got, c.want)
		}
	}
	if (Regime{}).WaitsForUs(at(9, 1), jst) {
		t.Error("設定が空なのに待つ")
	}
}

// TestMarginConfigWaitsForUs は本番（信用）の設定が土台の us_stale_wait_until を引き継ぐこと。
func TestMarginConfigWaitsForUs(t *testing.T) {
	cfg, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Regime.UsStaleWaitUntil != "09:12" {
		t.Errorf("us_stale_wait_until = %q, want 09:12", cfg.Regime.UsStaleWaitUntil)
	}
}
