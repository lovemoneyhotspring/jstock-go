package backtest

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
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

// 同じ業種の上限を超えた銘柄は落ち、次点が繰り上がる（signal.max_per_sector）。
// 業種が空の行は数に入れない——取れなかった銘柄どうしを同じ業種として束ねないため。
func TestPickDayLimitsPerSector(t *testing.T) {
	day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	row := func(code, sector string, gap float64) Row {
		return Row{Date: day, Code: code, Sector: sector, Open: 1000, Close: 1000,
			PrevClose: 1000 / (1 + gap), Gap: gap, Eligible: true}
	}
	rows := []Row{
		row("A", "2050", -0.05), // 建設
		row("B", "2050", -0.04), // 建設（上限で落ちる）
		row("C", "", -0.03),     // 業種が取れない
		row("D", "3600", -0.02), // 機械（繰り上がる）
	}
	p := legParams{n: 3, budget: decimal.NewFromInt(1_000_000), sign: 1,
		side: domain.SideBuy,
		signal: config.Signal{
			MinGap: decimal.NewFromInt(-1), MaxGap: decimal.Zero, RankBy: config.RankByGap,
			MaxPerSector: 1,
		},
		pick: selection.PickOptions{Weighting: "equal", Side: domain.SideBuy, MaxPerSector: 1},
	}
	got := pickDay(rows, p, p.n, p.budget)
	var codes []string
	for _, tr := range got {
		codes = append(codes, tr.Code)
	}
	want := []string{"A", "C", "D"}
	if len(codes) != len(want) {
		t.Fatalf("選ばれた銘柄 = %v, 期待 %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("選ばれた銘柄 = %v, 期待 %v", codes, want)
			break
		}
	}
}

// ショートだけ休む日は、ロングはそのままでショートの倍率だけ 0（本番の execute.SizeDay と同じ）。
func TestSeesawScalesShortOff(t *testing.T) {
	m := config.Default().Margin
	m.MultiplierNormal = decimal.NewFromInt(1)
	v := regime.Verdict{Trade: true, Scale: 1, ShockLong: 1, ShockShort: 1, ShortOff: true}
	long, short := seesawScales(v, m)
	if long != 1 || short != 0 {
		t.Errorf("long=%v short=%v, want 1 / 0", long, short)
	}
}

// ショートの一時停止中は「ショートだけ休む」日も倍率を残す（枠をロングへ回すため。execute.SizeDay と同じ）。
// ショック日の 0 倍はそのまま。
func TestSeesawScalesPausedKeepsShortOffBudget(t *testing.T) {
	m := config.Default().Margin
	m.MultiplierNormal = decimal.NewFromInt(1)
	m.Paused = true
	v := regime.Verdict{Trade: true, Scale: 1, ShockLong: 1, ShockShort: 1, ShortOff: true}
	if long, short := seesawScales(v, m); long != 1 || short != 1 {
		t.Errorf("ショートだけ休む日: long=%v short=%v, want 1 / 1", long, short)
	}
	v = regime.Verdict{Trade: true, Scale: 1, ShockLong: 1.5, ShockShort: 0, Shock: true}
	if long, short := seesawScales(v, m); long != 1.5 || short != 0 {
		t.Errorf("ショック日: long=%v short=%v, want 1.5 / 0", long, short)
	}
}

// 規則 R（capacity_ratio）の検証のショック日は、本番の保証金が読めない朝と同じく長短の固定合計で
// 頭打ちにする（2026-09-25 のレビュー。従来はロング 500 万 × 1.5 = 750 万で、本番の 700 万を超えていた）
func TestShockTotalCapMatchesLiveFallback(t *testing.T) {
	cfg, err := config.Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.CapacityRatio.IsPositive() {
		t.Skip("capacity_ratio を置かない設定")
	}
	fixed := cfg.Capital.MaxCapital.Add(cfg.Margin.MaxCapital)
	limit := shockTotalCap(cfg)
	if !limit.Equal(fixed) {
		t.Fatalf("ショック日の上限 %s, want 長短の固定合計 %s", limit, fixed)
	}
	n := cfg.Capital.Positions()
	shockLong, _ := cfg.Regime.ShockLongScale.Float64()
	shock := regime.Verdict{Trade: true, Scale: 1, Shock: true, ShockLong: shockLong}
	b := preScaledBudget(cfg.Capital.BudgetPerOrder(), shock, cfg.Margin.LongShrink, n, limit)
	if total := b.Mul(decimal.NewFromInt(int64(n))); total.GreaterThan(fixed) {
		t.Errorf("ショック日の総額 %s が固定合計 %s を超えた", total, fixed)
	}
	// 平日は頭打ちしない
	normal := regime.Verdict{Trade: true, Scale: 1, ShockLong: 1}
	if b := preScaledBudget(cfg.Capital.BudgetPerOrder(), normal, cfg.Margin.LongShrink, n, limit); !b.Equal(cfg.Capital.BudgetPerOrder()) {
		t.Errorf("平日の予算 %s, want %s", b, cfg.Capital.BudgetPerOrder())
	}
	// 比を置かない設定は上限なし（本番も ShockTotalCap を入れない）
	plain := cfg
	plain.Margin.CapacityRatio = decimal.Zero
	if got := shockTotalCap(plain); !got.IsZero() {
		t.Errorf("比の無い設定の上限 %s, want 0", got)
	}
}

// 規則 R の選ぶ前の予算は本番（execute.SizeDay）と同じ丸め: 段ごとに Round(0)。
// 掛け合わせてから Floor だと 1〜2 円ずれる
func TestPreScaledBudgetRoundsLikeLive(t *testing.T) {
	d := decimal.NewFromInt
	budget := d(691_335) // 11,150,590 × 62% ÷ 10 の端数つき
	for _, c := range []struct {
		name   string
		v      regime.Verdict
		shrink bool
		want   decimal.Decimal
	}{
		{"平日", regime.Verdict{Trade: true, Scale: 1, ShockLong: 1}, true, budget},
		// 691,335 × 0.7 = 483,934.5 → 483,935（掛け合わせて Floor なら 483,934）
		{"縮小", regime.Verdict{Trade: true, Scale: 0.7, ShockLong: 1}, true, d(483_935)},
		{"縮小しない設定", regime.Verdict{Trade: true, Scale: 0.7, ShockLong: 1}, false, budget},
		// 483,935 × 1.5 = 725,902.5 → 725,903（0.7 × 1.5 = 1.05 を掛けて Floor なら 725,901）
		{"縮小のショック日", regime.Verdict{Trade: true, Scale: 0.7, Shock: true, ShockLong: 1.5}, true, d(725_903)},
		{"止める日", regime.Verdict{Trade: false, Scale: 1, ShockLong: 1}, true, decimal.Zero},
	} {
		if got := preScaledBudget(budget, c.v, c.shrink, 10, decimal.Zero); !got.Equal(c.want) {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
