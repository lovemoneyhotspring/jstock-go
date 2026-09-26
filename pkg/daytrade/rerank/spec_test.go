package rerank

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// repoRoot はリポジトリの根（照合用データの spec はここからの相対パス）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// TestSpecParityWithPython は束（マニフェスト）ごとに、順位化と推論（bagging の平均）が学習側と一致するか。
// testdata/parity_<名前>.json は test/dt_lgbm_train_spec.py が束と同時に書く。
func TestSpecParityWithPython(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("testdata", "parity_*.json"))
	if len(files) == 0 {
		t.Fatal("束の照合用データが無い")
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var pf struct {
				Spec string `json:"spec"`
				parityFile
			}
			if err := json.Unmarshal(raw, &pf); err != nil {
				t.Fatal(err)
			}
			spec, err := LoadSpec(filepath.Join(repoRoot(t), pf.Spec))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(pf.Features, spec.Features) {
				t.Fatalf("特徴量の並びがマニフェストと違います: %v / %v", pf.Features, spec.Features)
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
							t.Fatalf("%s %s の %s: 順位 %v、学習側は %v", c.Day, r.Symbol, spec.Features[j], ranked[i][j], r.Ranked[j])
						}
					}
					sum := 0.0
					for _, m := range spec.models {
						sum += m.Predict(r.Ranked)
					}
					if got := sum / float64(len(spec.models)); math.Abs(got-r.Score) > 1e-12 {
						t.Fatalf("%s %s: 予測 %.17g、学習側は %.17g", c.Day, r.Symbol, got, r.Score)
					}
				}
			}
			if nanCells == 0 {
				t.Fatal("照合用データに欠損が無く、欠損の扱いを確かめられません")
			}
		})
	}
}

// 2 日続落の特徴量: 前日・前々日とも下げなら 1、片方でも取れない・下げでなければ 0。
func TestDown2AndRetD2(t *testing.T) {
	dn, up, dn2 := -0.01, 0.02, -0.03
	rows := []Input{
		{Ret1: &dn, RetD2: &dn2}, {Ret1: &dn, RetD2: &up}, {Ret1: &dn}, {RetD2: &dn2}, {Ret1: &up, RetD2: &dn2},
	}
	got := RawNamed(rows, []string{"ret_d2", "down2"})
	want := [][2]float64{{dn2, 1}, {up, 0}, {math.NaN(), 0}, {dn2, 0}, {dn2, 0}}
	for i := range rows {
		if !(got[i][0] == want[i][0] || math.IsNaN(got[i][0]) && math.IsNaN(want[i][0])) || got[i][1] != want[i][1] {
			t.Errorf("行 %d: %v、want %v", i, got[i], want[i])
		}
	}
}

// マニフェストの誤り（知らない特徴量・版の不足・入力の数の不一致）は読み込みで弾く。
func TestLoadSpecRejects(t *testing.T) {
	dir := t.TempDir()
	model := filepath.Join(repoRoot(t), "config", "daytrade", "models", "lgbm_rank.txt")
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	feats, _ := json.Marshal(FeatureNames)
	cases := map[string]string{
		"unknown.json": `{"features":["gap","nope"],"models":["` + model + `"],"plan_features":1}`,
		"version.json": `{"features":["gap","down2"],"models":["` + model + `"],"plan_features":1}`,
		"inputs.json":  `{"features":["gap","key"],"models":["` + model + `"],"plan_features":1}`,
		"empty.json":   `{"features":` + string(feats) + `,"models":[],"plan_features":1}`,
		"typo.json":    `{"features":` + string(feats) + `,"models":["` + model + `"],"plan_feature":1}`,
	}
	for name, body := range cases {
		if _, err := LoadSpec(write(name, body)); err == nil {
			t.Errorf("%s を読めてしまった", name)
		}
	}
	ok := write("ok.json", `{"name":"x","features":`+string(feats)+`,"models":["`+model+`","`+model+`"],"plan_features":1,"min_turnover":5e8}`)
	s, err := LoadSpec(ok)
	if err != nil || s.NumModels() != 2 || s.MinTurnover != 5e8 {
		t.Fatalf("正しい束を読めない: %v %+v", err, s)
	}
	// 従来の .txt は 1 本・FeatureNames・版 1・下限なし
	legacy, err := LoadSpec(model)
	if err != nil || legacy.NumModels() != 1 || legacy.PlanFeatures != 1 || legacy.MinTurnover != 0 {
		t.Fatalf("従来のモデルを束として読めない: %v %+v", err, legacy)
	}
}
