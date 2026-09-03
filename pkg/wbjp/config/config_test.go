package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
)

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadSettingsFileStopsDefaults(t *testing.T) {
	dir := writeSettings(t, "[universe]\nsymbols = [\"7203\"]\n")
	cfg, err := LoadSettingsFile(dir)
	if err != nil {
		t.Fatalf("読み込み失敗: %v", err)
	}
	if cfg.Stops.Trailing {
		t.Error("trailing の既定は false")
	}
	// 任意項目は「未設定」を nil で表す。0 で埋めると設定していない
	// 機能が動き出す。
	if cfg.Stops.BreakevenAfterR != nil || cfg.Stops.MaxHoldDays != nil ||
		cfg.Stops.StaleExitDays != nil || cfg.Stops.TakeProfitR != nil ||
		cfg.Stops.TrendExitSMA != nil || cfg.Stops.InitialStopPct != nil ||
		cfg.Stops.TrailingATRMultiple != nil || cfg.Stops.TrailingPct != nil {
		t.Errorf("未設定の項目は nil であるべき: %+v", cfg.Stops)
	}
	if !cfg.Stops.TakeProfitFraction.Equal(decimal.RequireFromString("0.5")) {
		t.Errorf("take_profit_fraction の既定は 0.5: %s", cfg.Stops.TakeProfitFraction)
	}
	if cfg.Stops.TrendExitKind != "sma" {
		t.Errorf("trend_exit_kind の既定は sma: %s", cfg.Stops.TrendExitKind)
	}
}

func TestLoadSettingsFileStopsValues(t *testing.T) {
	dir := writeSettings(t, `
[stops]
trailing = true
breakeven_after_r = 1.5
stale_exit_days = 10
max_hold_days = 40
initial_stop_pct = "0.04"
take_profit_r = 2
take_profit_fraction = 0.4
trend_exit_sma = 20
trend_exit_always = true
trend_exit_kind = "donchian"
trailing_atr_multiple = 2.5
trailing_pct = 0.08
`)
	cfg, err := LoadSettingsFile(dir)
	if err != nil {
		t.Fatalf("読み込み失敗: %v", err)
	}
	s := cfg.Stops
	if !s.Trailing || !s.TrendExitAlways {
		t.Error("bool 項目が読めていない")
	}
	if s.BreakevenAfterR == nil || !s.BreakevenAfterR.Equal(decimal.RequireFromString("1.5")) {
		t.Errorf("breakeven_after_r: %v", s.BreakevenAfterR)
	}
	if s.StaleExitDays == nil || *s.StaleExitDays != 10 || s.MaxHoldDays == nil || *s.MaxHoldDays != 40 {
		t.Error("日数の項目が読めていない")
	}
	if s.InitialStopPct == nil || !s.InitialStopPct.Equal(decimal.RequireFromString("0.04")) {
		t.Errorf("initial_stop_pct（文字列表記）: %v", s.InitialStopPct)
	}
	if s.TakeProfitR == nil || !s.TakeProfitR.Equal(decimal.NewFromInt(2)) {
		t.Errorf("take_profit_r（整数表記）: %v", s.TakeProfitR)
	}
	if !s.TakeProfitFraction.Equal(decimal.RequireFromString("0.4")) {
		t.Errorf("take_profit_fraction: %s", s.TakeProfitFraction)
	}
	if s.TrendExitSMA == nil || *s.TrendExitSMA != 20 || s.TrendExitKind != "donchian" {
		t.Error("trend_exit 系が読めていない")
	}
	if s.TrailingATRMultiple == nil || !s.TrailingATRMultiple.Equal(decimal.RequireFromString("2.5")) {
		t.Errorf("trailing_atr_multiple: %v", s.TrailingATRMultiple)
	}
	if s.TrailingPct == nil || !s.TrailingPct.Equal(decimal.RequireFromString("0.08")) {
		t.Errorf("trailing_pct: %v", s.TrailingPct)
	}
}

func TestLoadSettingsFileStopsValidation(t *testing.T) {
	cases := map[string]string{
		"未知の trend_exit_kind": "[stops]\ntrend_exit_kind = \"kijun\"\n",
		"R 倍率は正の数":            "[stops]\ntake_profit_r = 0\n",
		"日数は正の整数":             "[stops]\nmax_hold_days = 0\n",
		"比率は 1 未満":            "[stops]\ninitial_stop_pct = 1\n",
		"stale は max 以下":      "[stops]\nstale_exit_days = 50\nmax_hold_days = 40\n",
		"数値として読めない値は黙って無視せずエラーにする": "[stops]\ntake_profit_r = \"にばい\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSettingsFile(writeSettings(t, body)); err == nil {
				t.Error("エラーになるべき設定が通ってしまった")
			}
		})
	}
}

func TestLoadSettingsFileRegime(t *testing.T) {
	dir := writeSettings(t, `
[regime]
enabled = true
benchmark = "1306"
sma_long = 150
sma_mid = 40
slope_lookback = 15
exposure_bull = 1
exposure_caution = "0.4"
exposure_bear = 0
cash_yield_symbol = "^IRX"
`)
	cfg, err := LoadSettingsFile(dir)
	if err != nil {
		t.Fatalf("読み込み失敗: %v", err)
	}
	r := cfg.Regime
	if !r.Enabled || r.Benchmark != "1306" || r.SMALong != 150 || r.SMAMid != 40 || r.SlopeLookback != 15 {
		t.Errorf("regime が読めていない: %+v", r)
	}
	if !r.ExposureCaution.Equal(decimal.RequireFromString("0.4")) || !r.ExposureBear.IsZero() {
		t.Errorf("露出が読めていない: %+v", r)
	}
	if r.CashYieldSymbol != "^IRX" {
		t.Errorf("cash_yield_symbol: %s", r.CashYieldSymbol)
	}
}

func TestLoadSettingsFileRegimeDefaultsAndValidation(t *testing.T) {
	cfg, err := LoadSettingsFile(writeSettings(t, "[universe]\nsymbols = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Regime
	if r.Enabled || r.Benchmark != "SPY" || r.SMALong != 200 || r.SMAMid != 50 || r.SlopeLookback != 20 {
		t.Errorf("regime の既定値: %+v", r)
	}
	if !r.ExposureBull.Equal(decimal.NewFromInt(1)) || !r.ExposureCaution.Equal(decimal.RequireFromString("0.5")) {
		t.Errorf("露出の既定値: %+v", r)
	}

	bad := map[string]string{
		"露出は 0〜1":            "[regime]\nexposure_bull = 1.5\n",
		"弱気 ≤ 警戒 ≤ 強気の順":     "[regime]\nexposure_bull = 0.2\nexposure_caution = 0.8\n",
		"sma_mid < sma_long": "[regime]\nsma_mid = 200\nsma_long = 100\n",
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadSettingsFile(writeSettings(t, body)); err == nil {
				t.Error("エラーになるべき設定が通ってしまった")
			}
		})
	}
}

func TestLoadSettingsFileSymbolsFile(t *testing.T) {
	dir := writeSettings(t, "[universe]\nsymbols = [\"7203\"]\nsymbols_file = \"symbols.txt\"\n")
	body := "# コメント行\n6758  # ソニー\n7203\n\n9984\n6758\n"
	if err := os.WriteFile(filepath.Join(dir, "symbols.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadSettingsFile(dir)
	if err != nil {
		t.Fatalf("読み込み失敗: %v", err)
	}
	// symbols は allowlist でもあるので、ファイルの銘柄が合流していないと
	// リスク判定で全部弾かれる。順序は「既存 → ファイル」。
	want := []string{"7203", "6758", "9984"}
	if len(cfg.Universe.Symbols) != len(want) {
		t.Fatalf("銘柄数が合わない: %v", cfg.Universe.Symbols)
	}
	for i, sym := range want {
		if cfg.Universe.Symbols[i] != sym {
			t.Errorf("[%d] = %s, want %s（%v）", i, cfg.Universe.Symbols[i], sym, cfg.Universe.Symbols)
		}
	}
}

func TestLoadSettingsFileSymbolsFileMissingIsError(t *testing.T) {
	// 黙って空にすると「対象銘柄ゼロ」で静かに何も起きなくなる。
	dir := writeSettings(t, "[universe]\nsymbols_file = \"nope.txt\"\n")
	if _, err := LoadSettingsFile(dir); err == nil {
		t.Error("銘柄リストが無いのにエラーにならなかった")
	}
}

func TestLoadSettingsFileUniverseDefaults(t *testing.T) {
	cfg, err := LoadSettingsFile(writeSettings(t, "[universe]\nsymbols = [\"7203\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Universe.Market != "JP" {
		t.Errorf("market の既定は JP: %s", cfg.Universe.Market)
	}
	if cfg.Universe.DataProvider != "jquants" {
		t.Errorf("data_provider の既定は jquants: %s", cfg.Universe.DataProvider)
	}
}

func TestLoadSettingsFileRealConfig(t *testing.T) {
	// リポジトリ同梱の設定が読めなくなっていないか。
	if _, err := LoadSettingsFile("../../../config"); err != nil {
		t.Fatalf("config/settings.toml が読めない: %v", err)
	}
}
