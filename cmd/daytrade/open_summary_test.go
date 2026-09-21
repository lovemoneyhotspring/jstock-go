package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/shopspring/decimal"

	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
)

// open の要約（summary）に足した項目は、OpenRunSchema に列が無いと OpenRunFrame が黙って捨てる。
// us_low は 2026-09-18 から、preopen_limit_pct と opening_limit_dropped は 2026-09-21 から、
// 要約には入れているのに履歴に残っていなかった（2026-09-21 の整合の点検）。
func TestOpenSummaryKeysAreInOpenRunSchema(t *testing.T) {
	src, err := os.ReadFile("open.go")
	if err != nil {
		t.Fatal(err)
	}
	columns := map[string]bool{}
	for _, column := range dthistory.OpenRunSchema {
		columns[column.Name] = true
	}
	seen := 0
	for _, m := range regexp.MustCompile(`summary\["([a-z_]+)"\]`).FindAllSubmatch(src, -1) {
		seen++
		if name := string(m[1]); !columns[name] {
			t.Errorf("summary[%q] は OpenRunSchema に列が無く、履歴に残らない", name)
		}
	}
	if seen == 0 {
		t.Fatal("open.go に summary[...] が見つからない（試験の正規表現が古い）")
	}
}

func TestOpenRunFrameKeepsUsLowColumns(t *testing.T) {
	run := dthistory.OpenRunFrame(map[string]any{
		"us_low": true, "preopen_limit_pct": decimal.RequireFromString("1.5"), "opening_limit_dropped": 2,
	})
	row := run.Rows[0]
	if row["us_low"] != true {
		t.Errorf("us_low = %v", row["us_low"])
	}
	if row["preopen_limit_pct"] != 1.5 {
		t.Errorf("preopen_limit_pct = %v, want 1.5", row["preopen_limit_pct"])
	}
	if row["opening_limit_dropped"] != int64(2) {
		t.Errorf("opening_limit_dropped = %v, want 2", row["opening_limit_dropped"])
	}
}
