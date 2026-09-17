package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func repoModel(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "daytrade", "models", "lgbm_rank.txt")
}

func TestValidateRankByLGBM(t *testing.T) {
	c := Default()
	c.Signal.RankBy = RankByLGBM
	if err := c.Validate(); err == nil {
		t.Error("モデル未指定で通った")
	}
	// 無いモデルでも設定の読み込みは通す（close・verify・guard を止めない）。open が ModelError で確かめる
	c.Signal.Model = filepath.Join(t.TempDir(), "none.txt")
	if err := c.Validate(); err != nil {
		t.Errorf("無いモデルで設定の読み込みが止まった: %v", err)
	}
	if err := c.Signal.ModelError(); err == nil {
		t.Error("無いモデルで ModelError が nil")
	}
	c.Signal.Model = repoModel(t)
	if err := c.Validate(); err != nil {
		t.Errorf("本番のモデルで通らない: %v", err)
	}
	if err := c.Signal.ModelError(); err != nil {
		t.Errorf("本番のモデルで ModelError: %v", err)
	}
}

// LightGBM で並べられない日は gap_vol に戻し、米国小幅高の日は両脚とも休む。
func TestFallbackToGapVol(t *testing.T) {
	c := Default()
	c.Signal.RankBy = RankByLGBM
	c.Regime.UsSkipLegs = UsSkipLegsShort
	f := c.FallbackToGapVol()
	if f.Signal.RankBy != RankByGapVol || f.Regime.UsSkipLegs != UsSkipLegsAll {
		t.Errorf("rank_by=%s us_skip_legs=%s", f.Signal.RankBy, f.Regime.UsSkipLegs)
	}
	if c.Signal.RankBy != RankByLGBM || c.Regime.UsSkipLegs != UsSkipLegsShort {
		t.Error("元の設定が書き換わった")
	}
}

// signal.model の相対パスは、それを書いたファイルのディレクトリから解く（継いだ子の設定でも同じファイル）。
func TestSignalModelResolvesFromDefiningFile(t *testing.T) {
	root := t.TempDir()
	base, child := filepath.Join(root, "base"), filepath.Join(root, "child")
	for _, d := range []string{filepath.Join(base, "models"), child} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(repoModel(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "models", "m.txt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(dir, body string) {
		if err := os.WriteFile(filepath.Join(dir, Filename), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(base, "[signal]\nrank_by = \"lgbm\"\nmodel = \"models/m.txt\"\n")
	write(child, "extends = \"../base\"\n")
	for _, dir := range []string{base, child} {
		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if want := filepath.Join(base, "models", "m.txt"); cfg.Signal.Model != want {
			t.Errorf("%s: model = %s, want %s", dir, cfg.Signal.Model, want)
		}
	}
}
