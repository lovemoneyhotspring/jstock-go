package backtest

import (
	"testing"
	"time"
)

func TestRecentWindowIgnoresUntradedWindow(t *testing.T) {
	// 20 日すべて止めた（12 月を休んだ）直後: 合計は 0 だが「建てていない」ので nil
	pnl := make([]float64, 25)
	scales := make([]float64, 25)
	traded := make([]bool, 25)
	if got := recentWindow(pnl, scales, traded, 20, 20); got != nil {
		t.Errorf("建てていない窓で判定している: %v", *got)
	}
	// 窓の途中まで無くても、1 日でも建てていれば合計を返す（止めた日は 0 で数える）
	pnl[10], scales[10], traded[10] = -1000, 0.5, true
	pnl[11], scales[11], traded[11] = 3000, 1, true
	got := recentWindow(pnl, scales, traded, 20, 20)
	if got == nil || *got != -500+3000 {
		t.Errorf("直近損益 = %v, want 2500", got)
	}
	// 窓が揃う前は nil
	if got := recentWindow(pnl, scales, traded, 19, 20); got != nil {
		t.Errorf("窓が揃う前に判定している: %v", *got)
	}
}

func TestApplyCarryBothLegs(t *testing.T) {
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	nextOpen := 650.0
	rows := map[string]Row{
		// 前日終値 1000 → 制限値幅 ±300。引け 700 はストップ安、1300 はストップ高
		"2026-01-05|A": {Date: day, Code: "A", Open: 970, Close: 700, LimitLow: 700, LimitHigh: 1300, NextOpen: &nextOpen},
		"2026-01-05|B": {Date: day, Code: "B", Open: 1080, Close: 1300, LimitLow: 700, LimitHigh: 1300, NextOpen: &nextOpen},
	}
	long := []Trade{{Date: day, Code: "A", Shares: 100, Entry: 970, Exit: 700, Gross: -27_000, Fees: 100, PnL: -27_100}}
	long = applyCarry(long, rows, 1, 1)
	if !long[0].Carried {
		t.Fatal("引けストップ安のロングが持ち越しになっていない")
	}
	// 翌寄り 650 で売った: 100 × (650 − 970) = −32,000、手数料 100
	if long[0].Gross != -32_000 || long[0].PnL != -32_100 {
		t.Errorf("ロングの持ち越し損益 = %v / %v, want -32000 / -32100", long[0].Gross, long[0].PnL)
	}

	short := []Trade{{Date: day, Code: "B", Shares: 100, Entry: 1080, Exit: 1300, Gross: -22_000, Fees: 50, PnL: -22_050}}
	short = applyCarry(short, rows, -1, 0.5)
	if !short[0].Carried {
		t.Fatal("引けストップ高のショートが持ち越しになっていない")
	}
	// 係数 0.5: 差分 100 × (1300 − 650) × 0.5 = +32,500 を返済側の損益に足す
	if short[0].Gross != -22_000+32_500 {
		t.Errorf("ショートの持ち越し損益 = %v, want 10500", short[0].Gross)
	}

	// 張り付いていない取引は触らない
	untouched := []Trade{{Date: day, Code: "A", Shares: 100, Entry: 970, Exit: 700, Gross: -27_000}}
	untouched = applyCarry(untouched, rows, -1, 1) // ロングの行にショートの判定
	if untouched[0].Carried {
		t.Error("ストップ高でないのにショートを持ち越している")
	}
}

func TestScaleTradesAppliesDailyScale(t *testing.T) {
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	off := day.AddDate(0, 0, 1)
	trades := []Trade{
		{Date: day, Shares: 200, Amount: 200_000, Fees: 20, Commission: 10, Gross: 2000, PnL: 1980, Scale: 1},
		{Date: off, Shares: 100, Amount: 100_000, Fees: 10, Gross: 500, PnL: 490, Scale: 1},
	}
	got := scaleTrades(trades, map[string]float64{"2026-01-05": 0.5, "2026-01-06": 0})
	if len(got) != 1 {
		t.Fatalf("止めた日の取引が残っている: %d 件", len(got))
	}
	if got[0].Scale != 0.5 || got[0].Shares != 100 || got[0].PnL != 990 || got[0].Commission != 5 {
		t.Errorf("倍率が掛かっていない: %+v", got[0])
	}
}
