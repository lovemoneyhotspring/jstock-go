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

	// 前日に追証 → 建てない。当日の値が読めない朝に分かっている最新の値は「追証」で、
	// 固定値の満額に戻すのは逆向き（2026-09-25 のレビュー。従来は固定値で建てていた）
	short := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(6_000_000), Fusokugaku: decimal.NewFromInt(1)}
	out, total, _ = ratioFallbackConfig(cfg, &short)
	if !total.IsZero() || out.Capital.Positions() != 0 || out.Margin.Positions() != 0 {
		t.Errorf("前日に追証: total %s N %d/%d, want 0（建てない）", total, out.Capital.Positions(), out.Margin.Positions())
	}
	// 前日の建可能額 0 → 建てない
	zero := margincap.Snapshot{SinyouSinkidate: decimal.Zero}
	out, total, _ = ratioFallbackConfig(cfg, &zero)
	if !total.IsZero() || out.Capital.Positions() != 0 || out.Margin.Positions() != 0 {
		t.Errorf("前日の建可能額 0: total %s N %d/%d, want 0（建てない）", total, out.Capital.Positions(), out.Margin.Positions())
	}
	// 前日の建可能額が 188 万（ロングが order_budget の半分を下回る）→ 満額でなく前日の値で決め直す（R1）
	tiny := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(1_880_000)}
	out, total, fromStale = ratioFallbackConfig(cfg, &tiny)
	if !fromStale || !total.LessThan(fixed) || !out.Capital.MaxCapital.LessThan(cfg.Capital.MaxCapital) || out.Validate() != nil {
		t.Errorf("前日が 188 万: total %s fromStale %v long %s, want 固定値未満", total, fromStale, out.Capital.MaxCapital)
	}
}

// 規則 R で建可能額が小さい朝（ロング < order_budget の半分）も検証を通り、元の満額に戻らない（R1）。
// 従来は Validate の fees.PositionsFor が落ち、applyMarginCap が元の設定（満額・ShockTotalCap 0）を返していた
func TestRatioSmallCapacityStaysSmall(t *testing.T) {
	cfg, err := dtconfig.Load("../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	if !cfg.Margin.CapacityRatio.IsPositive() {
		t.Skip("規則 R（capacity_ratio）でない設定")
	}
	half := cfg.Capital.OrderBudget.Div(decimal.NewFromInt(2))
	for _, cap := range []int64{1_880_000, 1_900_000, 500_000} {
		snap := margincap.Snapshot{SinyouSinkidate: decimal.NewFromInt(cap)}
		capped, res := margincap.Apply(cfg, snap)
		long := capped.Capital.MaxCapital
		if !long.IsPositive() || !long.LessThan(cfg.Capital.MaxCapital) {
			t.Errorf("建可能額 %d: ロング %s, want 0 < ロング < %s", cap, long, cfg.Capital.MaxCapital)
		}
		if cap == 1_880_000 && !long.LessThan(half) {
			t.Errorf("建可能額 188 万のロング %s は order_budget の半分 %s を下回るはず（境界の確認）", long, half)
		}
		if cap == 1_900_000 && long.LessThan(half) {
			t.Errorf("建可能額 190 万のロング %s は order_budget の半分 %s 以上のはず（境界の確認）", long, half)
		}
		if err := capped.Validate(); err != nil {
			t.Errorf("建可能額 %d: 決め直した設定が検証を通らない: %v", cap, err)
		}
		if res.WatchOnly || capped.Capital.Positions() != cfg.Capital.MaxPositions {
			t.Errorf("建可能額 %d: N %d, want max_positions %d", cap, capped.Capital.Positions(), cfg.Capital.MaxPositions)
		}
		total := long.Add(capped.Margin.MaxCapital)
		if want := snap.SinyouSinkidate.Mul(cfg.Margin.CapacityRatio).Floor(); !total.Equal(want) {
			t.Errorf("建可能額 %d: 長短合計 %s, want %s", cap, total, want)
		}
		if !capped.Capital.ShockTotalCap.IsPositive() {
			t.Errorf("建可能額 %d: ショック日の頭打ちが入っていない", cap)
		}
	}
}

// 決め直した設定が検証を通らないときは建てない設定を返す。元の設定（満額）は返さない（R1）
func TestValidOrWatchOnly(t *testing.T) {
	cfg, err := dtconfig.Load("../../config/daytrade_margin")
	if err != nil {
		t.Fatalf("本番の設定を読めない: %v", err)
	}
	bad := cfg
	bad.Capital.MaxOrder = decimal.NewFromInt(-1)
	out, err := validOrWatchOnly(cfg, bad)
	if err == nil {
		t.Fatal("通らない設定でエラーが返らない")
	}
	if out.Capital.Positions() != 0 || out.Margin.Positions() != 0 || !out.Capital.MaxCapital.IsZero() {
		t.Errorf("検証失敗の戻り値が建てる設定: long %s N %d/%d", out.Capital.MaxCapital, out.Capital.Positions(), out.Margin.Positions())
	}
	if err := out.Validate(); err != nil {
		t.Errorf("建てない設定そのものが検証を通らない: %v", err)
	}
	good, err := validOrWatchOnly(cfg, cfg)
	if err != nil || !good.Capital.MaxCapital.Equal(cfg.Capital.MaxCapital) {
		t.Errorf("通る設定を変えた: %v %s", err, good.Capital.MaxCapital)
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
