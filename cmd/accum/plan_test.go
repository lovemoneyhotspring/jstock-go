package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// writePlanBars は from〜to の毎日に終値 closePrice の足を書く。
func writePlanBars(t *testing.T, store *data.BarStore, symbol, from, to string, closePrice int64) {
	t.Helper()
	start, _ := time.Parse("2006-01-02", from)
	end, _ := time.Parse("2006-01-02", to)
	c := decimal.NewFromInt(closePrice)
	var bars []domain.Bar
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		bar, err := domain.NewBar(symbol, d.Format("2006-01-02"), c, c, c, c, decimal.NewFromInt(1000))
		if err != nil {
			t.Fatal(err)
		}
		bars = append(bars, bar)
	}
	if err := store.Write(symbol, bars); err != nil {
		t.Fatal(err)
	}
}

// 判定用の足が無い戦略は、銘柄自身の足で黙って計算せず「見送り（判定用の足なし）」と出す。
// run は A7 で見送るので、plan だけ額を出すと run と食い違う。
func TestPrintPlanSkipsWhenSignalBarsMissing(t *testing.T) {
	store := data.NewBarStore(t.TempDir())
	writePlanBars(t, store, "1306.T", "2026-08-25", "2026-09-13", 1000)
	writePlanBars(t, store, "2559.T", "2026-08-25", "2026-09-13", 1000)
	cfg := &accumcfg.AccumConfig{Tactics: []accumcfg.TacticEntry{
		{ID: "A", Tactic: "constant", Symbols: []string{"1306.T"}, MonthlyBudget: decimal.NewFromInt(100_000)},
		{ID: "B", Tactic: "constant", Symbols: []string{"2559.T"}, MonthlyBudget: decimal.NewFromInt(100_000),
			SignalSymbol: "^IXIC", SignalMarket: "US"},
	}}

	var out bytes.Buffer
	now := time.Date(2026, 9, 14, 1, 0, 0, 0, time.UTC)
	if err := printPlan(&out, cfg, store, now, 3); err != nil {
		t.Fatalf("plan が失敗: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "見送り（判定用の足なし）: 2559.T（^IXIC）") {
		t.Errorf("判定用の足なしの見送りが出ていない:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "2559.T") && strings.HasPrefix(line, "2026-") {
			t.Errorf("判定用の足が無いのに額を出した: %s", line)
		}
	}
	if !strings.Contains(got, "1306.T") {
		t.Errorf("判定用の足を使わない戦略まで消えた:\n%s", got)
	}

	// 判定用の足があれば、いつもどおり額を出す
	writePlanBars(t, store, "^IXIC", "2026-08-25", "2026-09-13", 20000)
	out.Reset()
	if err := printPlan(&out, cfg, store, now, 3); err != nil {
		t.Fatalf("plan が失敗: %v", err)
	}
	if strings.Contains(out.String(), "判定用の足なし") {
		t.Errorf("判定用の足があるのに見送りと出た:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "2026-09-13  2559.T") {
		t.Errorf("判定用の足があるのに額が出ていない:\n%s", out.String())
	}
}
