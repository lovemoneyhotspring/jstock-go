package rerank

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// modelPath は本番が読むモデル（config/daytrade/models/lgbm_rank.txt）。
func modelPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "daytrade", "models", "lgbm_rank.txt")
}

type parityRow struct {
	Symbol   string     `json:"symbol"`
	RuleRank int        `json:"rule_rank"`
	Raw      []*float64 `json:"raw"`
	Ranked   []float64  `json:"ranked"`
	Score    float64    `json:"score"`
}

type parityFile struct {
	Features []string `json:"features"`
	Cases    []struct {
		Day  string      `json:"day"`
		Rows []parityRow `json:"rows"`
	} `json:"cases"`
}

// TestParityWithPython は順位化と推論が学習側（Python の pandas・LightGBM）と一致するか。
// testdata/parity.json は test/dt_lgbm_train.py がモデルと同時に書く。
func TestParityWithPython(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pf parityFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pf.Features, FeatureNames) {
		t.Fatalf("特徴量の並びが学習側と違います: %v", pf.Features)
	}
	m, err := Cached(modelPath(t))
	if err != nil {
		t.Fatal(err)
	}
	nanCells := 0
	for _, c := range pf.Cases {
		values := make([][]float64, len(c.Rows))
		for i, r := range c.Rows {
			values[i] = make([]float64, len(r.Raw))
			for j, v := range r.Raw {
				if v == nil {
					values[i][j] = math.NaN()
					nanCells++
					continue
				}
				values[i][j] = *v
			}
		}
		ranked := Ranked(values)
		for i, r := range c.Rows {
			for j := range r.Ranked {
				if ranked[i][j] != r.Ranked[j] {
					t.Fatalf("%s %s の %s: 順位 %v、学習側は %v", c.Day, r.Symbol, FeatureNames[j], ranked[i][j], r.Ranked[j])
				}
			}
			// 推論は学習側の順位化の値から（順位化の一致は上で見た）
			if got := m.Predict(r.Ranked); math.Abs(got-r.Score) > 1e-12 {
				t.Fatalf("%s %s: 予測 %.17g、学習側は %.17g", c.Day, r.Symbol, got, r.Score)
			}
		}
	}
	if nanCells == 0 {
		t.Fatal("照合用データに欠損が無く、欠損の扱いを確かめられません")
	}
}

// TestRawMatchesTraining は Input から作る生の特徴量が、学習側の定義と同じ式か。
func TestRawMatchesTraining(t *testing.T) {
	vol, ret, si := 0.01, 0.03, 0.004
	rows := []Input{
		{Gap: -0.03125, Price: 968, RuleRank: 1, TurnoverMed: 5e8, MktCap: 1e5, Vol20: &vol, Ret1: &ret, ShortInterest: &si},
		{Gap: -0.01, Price: 99, RuleRank: 2, TurnoverMed: 2e8, MktCap: 0},
	}
	got := Raw(rows)
	// round(-0.03125, 4) は偶数丸めで -0.0312、ボラは下限 0.02
	if want := -0.0312 / 0.02; math.Abs(got[0][1]-want) > 1e-15 {
		t.Fatalf("key = %v、want %v", got[0][1], want)
	}
	if !math.IsNaN(got[1][1]) || !math.IsNaN(got[1][8]) || !math.IsNaN(got[1][10]) {
		t.Fatalf("ボラ・時価総額が無い行は key・turn_cap・log_cap が NaN のはず: %v", got[1])
	}
	if got[0][8] != 5e8/1e5 || got[0][14] != 2 || got[1][15] != 1 {
		t.Fatalf("turn_cap / n_cand / rank_pct が違います: %v %v", got[0], got[1])
	}
	r := Ranked(got)
	// n_cand は全員同順位 → (1+2)/2/2
	if r[0][14] != 0.75 || r[1][14] != 0.75 {
		t.Fatalf("同順位の百分位 = %v %v", r[0][14], r[1][14])
	}
	// 欠損は 0.5、1 件だけの列は 1
	if r[1][3] != 0.5 || r[0][3] != 1 {
		t.Fatalf("欠損の埋め方が違います: %v %v", r[0][3], r[1][3])
	}
}

func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "none.txt")); err == nil {
		t.Fatal("無いファイルで誤りにならない")
	}
}

// 節の番号が壊れたモデルは読み込みで弾く（predict が範囲の外を読む・回り続けるのを防ぐ）。
func TestLoadRejectsBrokenTree(t *testing.T) {
	const head = "tree_sizes=100\nmax_feature_idx=1\nobjective=regression\n\nTree=0\nnum_leaves=3\nnum_cat=0\n" +
		"threshold=0.5 0.5\ndecision_type=2 2\nleaf_value=1 2 3\n"
	cases := map[string]string{
		"正常":       "split_feature=0 1\nleft_child=1 -1\nright_child=-2 -3\n",
		"子が親へ戻る":   "split_feature=0 1\nleft_child=1 0\nright_child=-2 -3\n",
		"子が範囲の外":   "split_feature=0 1\nleft_child=5 -1\nright_child=-2 -3\n",
		"葉が範囲の外":   "split_feature=0 1\nleft_child=1 -1\nright_child=-2 -9\n",
		"特徴量が範囲の外": "split_feature=0 7\nleft_child=1 -1\nright_child=-2 -3\n",
	}
	for name, body := range cases {
		path := filepath.Join(t.TempDir(), "model.txt")
		if err := os.WriteFile(path, []byte(head+body+"\nend of trees\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if name == "正常" && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if name != "正常" && err == nil {
			t.Errorf("%s: 誤りにならない", name)
		}
	}
}

// 木の境目で切れたモデル（tree_sizes より木が少ない）は読み込みで弾く。
func TestLoadRejectsTruncatedModel(t *testing.T) {
	full, err := os.ReadFile(modelPath(t))
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.LastIndex(full, []byte("\nTree="))
	if cut < 0 {
		t.Fatal("モデルに Tree= が無い")
	}
	path := filepath.Join(t.TempDir(), "model.txt")
	if err := os.WriteFile(path, full[:cut+1], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("途中で切れたモデルが誤りにならない")
	}
}
