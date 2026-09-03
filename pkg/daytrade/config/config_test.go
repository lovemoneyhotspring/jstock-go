package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
)

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadRealConfigs(t *testing.T) {
	// リポジトリに実在する設定が読めることは、移植の最低条件
	for _, dir := range []string{"../../../config/daytrade", "../../../config/daytrade_margin"} {
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if cfg.Capital.Positions() != 3 {
			t.Errorf("%s: N = %d, want 3", dir, cfg.Capital.Positions())
		}
		if len(cfg.Regime.SkipMonths) != 1 || cfg.Regime.SkipMonths[0] != 12 {
			t.Errorf("%s: skip_months = %v, want [12]", dir, cfg.Regime.SkipMonths)
		}
		// drift_gate はコメントアウトされている（既定は無効）
		if cfg.Regime.DriftGate != nil {
			t.Errorf("%s: drift_gate は無効のはず", dir)
		}
		if cfg.Regime.UsSkipHigh == nil {
			t.Errorf("%s: us_skip_high が読めていない", dir)
		}
	}
	margin, err := Load("../../../config/daytrade_margin")
	if err != nil {
		t.Fatal(err)
	}
	if !margin.Margin.Enabled || margin.Margin.Positions() != 3 {
		t.Errorf("ショートの N = %d（enabled=%v）, want 3 / true", margin.Margin.Positions(), margin.Margin.Enabled)
	}
	if margin.StrategyName() != "jp_gap_fade_margin" {
		t.Errorf("戦略名 = %s", margin.StrategyName())
	}
}

func TestBudgetPerOrder(t *testing.T) {
	cfg := Default()
	// 200 万 ÷ 3 = 666666.67 → 円未満切り捨て
	if got := cfg.Capital.BudgetPerOrder(); !got.Equal(decimal.NewFromInt(666_666)) {
		t.Errorf("BudgetPerOrder = %s, want 666666", got)
	}
	// 資金 0 は「買わない」（N = 0、予算 0）
	cfg.Capital.MaxCapital = decimal.Zero
	if cfg.Capital.Positions() != 0 || !cfg.Capital.BudgetPerOrder().IsZero() {
		t.Error("資金 0 で N・予算が 0 にならない")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	// 綴りを間違えた設定が「効いているつもり」で効かないまま本番に乗るのを防ぐ
	dir := write(t, "[capital]\nmax_capitl = 100\n")
	if _, err := Load(dir); err == nil {
		t.Error("未知の項目がエラーにならない")
	}
}

func TestValidateRanges(t *testing.T) {
	cases := map[string]string{
		"weighting が不正":             "[capital]\nweighting = \"random\"\n",
		"segments に未知の区分":           "[universe]\nsegments = [\"tokyo\"]\n",
		"exclude_cap_terciles が範囲外": "[universe]\nexclude_cap_terciles = 3\n",
		"skip_months が範囲外":          "[regime]\nskip_months = [13]\n",
		"equity_curve_scale が範囲外":   "[regime]\nequity_curve_scale = 1.5\n",
		"us_skip_high が low 以下":     "[regime]\nus_skip_low = 0.02\nus_skip_high = 0.01\n",
		"時間帯の開始が終了より後":              "[execution]\nentry_window = [\"09:30\", \"09:00\"]\n",
		"carry_penalty が範囲外":        "[margin]\ncarry_penalty = 2\n",
	}
	for name, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s がエラーにならない", name)
		}
	}
}

func TestWindow(t *testing.T) {
	cfg := Default()
	sh, sm, eh, em, err := cfg.Execution.Window("entry")
	if err != nil || sh != 9 || sm != 0 || eh != 9 || em != 15 {
		t.Errorf("entry_window = %d:%d〜%d:%d, %v", sh, sm, eh, em, err)
	}
	sh, sm, eh, em, err = cfg.Execution.Window("exit")
	if err != nil || sh != 15 || sm != 20 || eh != 15 || em != 30 {
		t.Errorf("exit_window = %d:%d〜%d:%d, %v", sh, sm, eh, em, err)
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("設定が無いのにエラーにならない")
	}
}
