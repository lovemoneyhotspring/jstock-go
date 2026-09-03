package config

import (
	"strings"
	"testing"
)

const basketBody = `
[[baskets]]
id = "TOPIX70+日経30"
weights = { "1306.T" = 0.7, "1321.T" = 0.3 }
tactic = "bear_stack"
multiplier = 2
monthly_budget = 50_000
benchmark = "1306.T"
tilt_strength = 2.0
`

func TestLoadBaskets(t *testing.T) {
	cfg, err := LoadAccumConfig(writeConfig(t, minimalHeader+basketBody))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Baskets) != 1 {
		t.Fatalf("バスケットが %d 件", len(cfg.Baskets))
	}
	b := cfg.Baskets[0]
	if b.MonthlyBudget.IntPart() != 50_000 {
		t.Errorf("monthly_budget = %s", b.MonthlyBudget)
	}
	if b.Benchmark != "1306.T" {
		t.Errorf("benchmark = %q", b.Benchmark)
	}
	if got := b.Symbols(); len(got) != 2 || got[0] != "1306.T" || got[1] != "1321.T" {
		t.Errorf("構成銘柄 = %v", got)
	}
	if b.TiltLookback != 252 {
		t.Errorf("tilt_lookback の既定は 252、実際は %d", b.TiltLookback)
	}
	// 倍率は [[tactics]] と同じ手順で反映される
	tactic, err := b.BuildTactic()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tactic.Describe(), "2") {
		t.Errorf("倍率が反映されていない: %s", tactic.Describe())
	}
	tilt, err := b.BuildTilt()
	if err != nil {
		t.Fatal(err)
	}
	if tilt == nil || tilt.Strength != 2.0 {
		t.Errorf("傾斜が反映されていない: %+v", tilt)
	}
	schedule, err := b.BuildSchedule()
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule.At("2026-01-05")["1306.T"]; got != 0.7 {
		t.Errorf("配分 = %v", got)
	}
	if len(cfg.ActiveBaskets()) != 1 {
		t.Errorf("有効なバスケットが %d 件", len(cfg.ActiveBaskets()))
	}
}

func TestBasketDefaults(t *testing.T) {
	cfg, err := LoadAccumConfig(writeConfig(t, minimalHeader+`
[[baskets]]
id = "既定"
weights = { "1306.T" = 1 }
`))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Baskets[0]
	if b.MonthlyBudget.IntPart() != 25_000 {
		t.Errorf("省略時は共通設定の予算: %s", b.MonthlyBudget)
	}
	if b.Tactic != "constant" {
		t.Errorf("戦略の既定は constant: %q", b.Tactic)
	}
	if b.Benchmark != "1306.T" {
		t.Errorf("基準銘柄の既定: %q", b.Benchmark)
	}
	tilt, err := b.BuildTilt()
	if err != nil || tilt != nil {
		t.Errorf("傾斜は既定で無効: %v %v", tilt, err)
	}
}

func TestBasketIDCollidesWithTacticID(t *testing.T) {
	_, err := LoadAccumConfig(writeConfig(t, minimalHeader+`
[[tactics]]
id = "同じ名前"
tactic = "constant"
symbols = ["1306.T"]
monthly_budget = 10_000

[[baskets]]
id = "同じ名前"
weights = { "1321.T" = 1 }
`))
	if err == nil || !strings.Contains(err.Error(), "id が重複") {
		t.Fatalf("戦略とバスケットで id が重なれば弾くはず: %v", err)
	}
}

// バスケットは比較検証が主用途なので、1銘柄が複数のバスケットに現れてよい。
func TestBasketsMaySharSymbols(t *testing.T) {
	cfg, err := LoadAccumConfig(writeConfig(t, minimalHeader+`
[[baskets]]
id = "A"
weights = { "1306.T" = 1 }

[[baskets]]
id = "B"
weights = { "1306.T" = 0.5, "1321.T" = 0.5 }
`))
	if err != nil {
		t.Fatalf("重なりは許されるはず: %v", err)
	}
	if len(cfg.Baskets) != 2 {
		t.Errorf("バスケットが %d 件", len(cfg.Baskets))
	}
}

func TestBasketRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"比率": `
[[baskets]]
id = "X"
weights = { "1306.T" = 0 }
`,
		"未知の戦略": `
[[baskets]]
id = "X"
weights = { "1306.T" = 1 }
tactic = "存在しない"
`,
		"予算": `
[[baskets]]
id = "X"
weights = { "1306.T" = 1 }
monthly_budget = -1
`,
	}
	for name, body := range cases {
		if _, err := LoadAccumConfig(writeConfig(t, minimalHeader+body)); err == nil {
			t.Errorf("%s: 弾かれるべき", name)
		}
	}
}

func TestBasketDisabled(t *testing.T) {
	cfg, err := LoadAccumConfig(writeConfig(t, minimalHeader+`
[[baskets]]
id = "止めてある"
weights = { "1306.T" = 1 }
enabled = false
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ActiveBaskets()) != 0 {
		t.Errorf("停止中は Active に入らない: %v", cfg.ActiveBaskets())
	}
}
