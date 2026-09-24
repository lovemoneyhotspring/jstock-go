package main

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/margincap"
)

// 規則 R で当日の保証金が読めない朝は、設定の固定値より大きく建てない（ショック日も）。
// 前の日のキャッシュで小さくなるならそちらを取る（2026-09-25 のレビュー）
func TestRatioFallbackOnlyLowers(t *testing.T) {
	cfg, err := dtconfig.Load("../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.CapacityRatio.IsPositive() {
		t.Skip("規則 R（capacity_ratio）でない設定")
	}
	fixed := cfg.Capital.MaxCapital.Add(cfg.Margin.MaxCapital)

	out, total, fromStale := ratioFallbackConfig(cfg, nil)
	if fromStale || !total.Equal(fixed) || !out.Capital.MaxCapital.Equal(cfg.Capital.MaxCapital) {
		t.Errorf("キャッシュ無し: total %s fromStale %v, want 固定値 %s", total, fromStale, fixed)
	}
	if !out.Capital.ShockTotalCap.Equal(fixed) {
		t.Errorf("キャッシュ無しのショック日の頭打ち %s, want %s", out.Capital.ShockTotalCap, fixed)
	}

	// 前日の建可能額が小さい（× capacity_ratio が固定値を下回る）→ そちらで決め直す
	small := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(6_000_000)}
	out, total, fromStale = ratioFallbackConfig(cfg, &small)
	if !fromStale || !total.LessThan(fixed) || out.Validate() != nil {
		t.Errorf("前日が小さい: total %s fromStale %v, want 固定値 %s 未満", total, fromStale, fixed)
	}
	if out.Capital.ShockTotalCap.GreaterThan(total) {
		t.Errorf("前日が小さい日のショック日の頭打ち %s が長短合計 %s を超えた", out.Capital.ShockTotalCap, total)
	}

	// 前日の建可能額が大きい → 固定値のまま（上げない）
	big := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(30_000_000)}
	if _, total, fromStale = ratioFallbackConfig(cfg, &big); fromStale || !total.Equal(fixed) {
		t.Errorf("前日が大きい: total %s fromStale %v, want 固定値 %s のまま", total, fromStale, fixed)
	}

	// 前日に追証 → 前日の値は使わず固定値（建てるかどうかは当日の値で決める）
	short := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(6_000_000), Fusokugaku: decimal.NewFromInt(1)}
	if _, _, fromStale = ratioFallbackConfig(cfg, &short); fromStale {
		t.Error("前日に追証のキャッシュで決め直した")
	}
}

// 順位表の無い日の作り直しは、その日の保証金の控えがあれば open と同じ総額で決め直す
func TestRankingConfigForUsesDatedSnapshot(t *testing.T) {
	cfg, err := dtconfig.Load("../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.CapacityRatio.IsPositive() {
		t.Skip("規則 R（capacity_ratio）でない設定")
	}
	saved := appSettings.DataDir
	appSettings.DataDir = t.TempDir()
	t.Cleanup(func() { appSettings.DataDir = saved })
	day := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)

	if got := rankingConfigFor(cfg, day); !got.Capital.MaxCapital.Equal(cfg.Capital.MaxCapital) {
		t.Errorf("控えが無い日に総額を変えた: %s", got.Capital.MaxCapital)
	}
	snap := margincap.Snapshot{Day: "2026-09-24", SinyouSinkidate: decimal.NewFromInt(11_150_590)}
	if err := margincap.Write(margincap.DatedCachePath(appSettings.DataDir, "2026-09-24"), snap); err != nil {
		t.Fatal(err)
	}
	got := rankingConfigFor(cfg, day)
	total := got.Capital.MaxCapital
	if got.Margin.Enabled {
		total = total.Add(got.Margin.MaxCapital)
	}
	if want := snap.SinyouSinkidate.Mul(cfg.Margin.CapacityRatio).Floor(); !total.Equal(want) {
		t.Errorf("控えのある日の長短合計 %s, want %s（建可能額 × capacity_ratio）", total, want)
	}
}
