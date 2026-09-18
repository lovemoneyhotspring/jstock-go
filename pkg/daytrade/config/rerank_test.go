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

// rank_by_us_low は米国小幅高の日だけ並べ方を替える（平常日は rank_by のまま）。
func TestRankForDay(t *testing.T) {
	c := Default()
	c.Signal.RankBy = RankByGapVol
	c.Signal.RankByUsLow = RankByLGBM
	if got := c.Signal.RankForDay(false); got != RankByGapVol {
		t.Errorf("平常日 = %s, want %s", got, RankByGapVol)
	}
	if got := c.Signal.RankForDay(true); got != RankByLGBM {
		t.Errorf("米国小幅高の日 = %s, want %s", got, RankByLGBM)
	}
	// 空なら両日とも rank_by
	c.Signal.RankByUsLow = ""
	if got := c.Signal.RankForDay(true); got != RankByGapVol {
		t.Errorf("rank_by_us_low が空の小幅高の日 = %s, want %s", got, RankByGapVol)
	}
	// ForDay は元の設定を書き換えない
	c.Signal.RankByUsLow = RankByLGBM
	if d := c.Signal.ForDay(true); d.RankBy != RankByLGBM || c.Signal.RankBy != RankByGapVol {
		t.Errorf("ForDay = %s, 元 = %s（元が書き換わった）", d.RankBy, c.Signal.RankBy)
	}
}

// rank_by_us_low = lgbm のときも、モデルが要るし、読めなければ gap_vol に落ちる。
func TestRankByUsLowNeedsModel(t *testing.T) {
	c := Default()
	c.Signal.RankBy = RankByGapVol
	c.Signal.RankByUsLow = RankByLGBM
	if err := c.Validate(); err == nil {
		t.Error("モデル未指定で通った")
	}
	c.Signal.Model = repoModel(t)
	if err := c.Validate(); err != nil {
		t.Errorf("モデルを指定しても通らない: %v", err)
	}
	if !c.Signal.UsesLGBM() {
		t.Error("UsesLGBM が偽（小幅高の日に使う設定）")
	}
	c.Regime.UsSkipLegs = UsSkipLegsShort
	f := c.FallbackToGapVol()
	if f.Signal.RankByUsLow != "" || f.Signal.RankBy != RankByGapVol || f.Regime.UsSkipLegs != UsSkipLegsAll {
		t.Errorf("フォールバック後: rank_by=%s rank_by_us_low=%q us_skip_legs=%s",
			f.Signal.RankBy, f.Signal.RankByUsLow, f.Regime.UsSkipLegs)
	}
	// 値の検証
	c.Signal.RankByUsLow = "いいかげんな値"
	if err := c.Validate(); err == nil {
		t.Error("知らない並べ方で通った")
	}
}
