package usmarket

import (
	"strings"
	"testing"
	"time"
)

const cboeSample = `{"timestamp": "2026-09-15 01:01:42", "symbol": "_SPX", "data": [
{"date": "2026-09-10", "open": "7600.0", "high": "7650.0", "low": "7590.0", "close": "7591.700000"},
{"date": "2026-09-11", "open": "7636.75", "high": "7677.02", "low": "7636.75", "close": "7656.980000"},
{"date": "2026-09-14", "open": "7611.44", "high": "7647.99", "low": "7592.28", "close": "7619.980000"},
{"date": "2026-09-15", "open": "7620.00", "high": "7630.00", "low": "7610.00", "close": "bad"}
]}`

func TestParseCboe(t *testing.T) {
	// 9:01 JST の 9/15 = NY の 9/14 20:01（引け後）
	now := time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC)
	got, err := parseCboe(strings.NewReader(cboeSample), day("2026-09-11"), day("2026-09-15"), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["2026-09-11"] != 7656.98 || got["2026-09-14"] != 7619.98 {
		t.Errorf("終値 = %v（範囲外の 9/10 と読めない 9/15 は入れない）", got)
	}
}

func TestParseCboeSkipsSessionInProgress(t *testing.T) {
	// NY の 9/14 15:30（取引時間中）に取ると 9/14 の行は途中の値
	now := time.Date(2026, 9, 14, 19, 30, 0, 0, time.UTC)
	got, err := parseCboe(strings.NewReader(cboeSample), day("2026-09-10"), day("2026-09-14"), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["2026-09-14"]; ok {
		t.Error("引けていない日の値を終値として入れた")
	}
	if _, ok := got["2026-09-11"]; !ok {
		t.Error("前日の終値が無い")
	}
}

func TestParseCboeEmptyIsAnError(t *testing.T) {
	// 範囲に 1 行も無ければ失敗（FirstOf が次の取得元へ回す）
	now := time.Date(2026, 9, 15, 0, 1, 0, 0, time.UTC)
	if _, err := parseCboe(strings.NewReader(cboeSample), day("2026-08-01"), day("2026-08-31"), now); err == nil {
		t.Error("終値が無いのにエラーにならない")
	}
	if _, err := parseCboe(strings.NewReader("<html>"), day("2026-09-01"), day("2026-09-15"), now); err == nil {
		t.Error("読めない応答がエラーにならない")
	}
}
