package main

import (
	"os"
	"path/filepath"
	"testing"

	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
)

func writeStrategies(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "strategies.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func buildFrom(t *testing.T, body string) ([]strategy.Strategy, map[string]float64, error) {
	t.Helper()
	dir := writeStrategies(t, body)
	saved := configDirFlag
	configDirFlag = dir
	t.Cleanup(func() { configDirFlag = saved })

	cfg, err := wbjpcfg.LoadStrategiesConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	return buildStrategies(cfg)
}

// TOML に書いたパラメータが実体に届くこと。switch でのハードコードに戻ると、
// 名前は合っているのに既定値で動く（最も気づきにくい事故）。
func TestBuildStrategiesAppliesTOMLParams(t *testing.T) {
	strats, weights, err := buildFrom(t, `
[[strategies]]
name = "atr_breakout"
weight = 1.5
channel = 30
atr_period = 21
min_atr_ratio = 0.02
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(strats) != 1 {
		t.Fatalf("戦略数 = %d", len(strats))
	}
	ab, ok := strats[0].(*strategy.ATRBreakout)
	if !ok {
		t.Fatalf("型が違う: %T", strats[0])
	}
	if ab.Channel != 30 || ab.ATRPeriod != 21 || ab.MinATRRatio != 0.02 {
		t.Errorf("パラメータが届いていない: %+v", ab)
	}
	if weights["atr_breakout"] != 1.5 {
		t.Errorf("weight = %g", weights["atr_breakout"])
	}
}

// 重みの書き忘れが 0.0（＝合成で無視）になると、有効にしたはずの戦略が
// 黙って効かなくなる。
func TestBuildStrategiesDefaultsWeightToOne(t *testing.T) {
	_, weights, err := buildFrom(t, "[[strategies]]\nname = \"sma_cross\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if weights["sma_cross"] != 1.0 {
		t.Errorf("weight = %g, 期待 1.0", weights["sma_cross"])
	}
}

func TestBuildStrategiesSkipsDisabled(t *testing.T) {
	strats, _, err := buildFrom(t, `
[[strategies]]
name = "sma_cross"
enabled = false

[[strategies]]
name = "rsi_reversion"
enabled = true
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(strats) != 1 || strats[0].Name() != "rsi_reversion" {
		t.Fatalf("有効な戦略だけ組み立てる: %v", strats)
	}
}

// 綴りを間違えたキーは黙って無視せずエラーにする。
func TestBuildStrategiesRejectsUnknownParam(t *testing.T) {
	if _, _, err := buildFrom(t, "[[strategies]]\nname = \"sma_cross\"\nfastt = 10\n"); err == nil {
		t.Error("知らないパラメータは弾くべき")
	}
}

func TestBuildStrategiesRejectsUnknownStrategy(t *testing.T) {
	if _, _, err := buildFrom(t, "[[strategies]]\nname = \"bear_stack\"\n"); err == nil {
		t.Error("知らない戦略名は弾くべき")
	}
}

// 同梱の設定ファイルが実際に読めること。
func TestShippedConfigBuilds(t *testing.T) {
	cfg, err := wbjpcfg.LoadStrategiesConfig("../../config")
	if err != nil {
		t.Skipf("設定ファイルが読めない: %v", err)
	}
	saved := configDirFlag
	configDirFlag = "../../config"
	t.Cleanup(func() { configDirFlag = saved })

	strats, weights, err := buildStrategies(cfg)
	if err != nil {
		t.Fatalf("同梱の strategies.toml が組み立てられない: %v", err)
	}
	if len(strats) == 0 {
		t.Fatal("戦略が 1 つも作られない")
	}
	for _, s := range strats {
		if weights[s.Name()] <= 0 {
			t.Errorf("%s の重みが %g", s.Name(), weights[s.Name()])
		}
		t.Logf("%s (weight=%g, warmup=%d)", s.Describe(), weights[s.Name()], s.WarmupBars())
	}
}
