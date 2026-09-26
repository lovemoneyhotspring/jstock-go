package rerank

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Spec はモデルの束。並べ替えに要るものをまとめて差し替えられるようにする:
// 特徴量の並び・モデル（複数なら予測を平均する。bagging）・plan の特徴量の版・並べる前の売買代金の下限。
//
// 設定の signal.model / model_us_low が指すファイルで形が決まる。
//   - .json はマニフェスト（manifest）。モデルのパスはマニフェストのディレクトリから
//   - それ以外は従来の 1 本のモデル（LightGBM のテキスト形式、特徴量は FeatureNames、版 1、下限なし）
//
// 学習し直したモデルは、Go の featureFuncs にある特徴量だけを使うなら、マニフェストと
// モデルのファイルを置き換えるだけで入る（bin の作り直しは要らない）。学習は test/dt_lgbm_train.py。
type Spec struct {
	// Path は読んだファイル、Name はマニフェストの名前（記録用）。
	Path string
	Name string
	// Features は入力の並び（featureFuncs の名前）。
	Features []string
	// PlanFeatures はこのモデルが要る plan の特徴量の版（plan.RerankFeaturesVersion の番号）。
	PlanFeatures int
	// MinTurnover は並べる前に候補から外す売買代金 20 日中央値の下限（円）。0 なら外さない。
	// 特徴量（順位・候補数）は外す前の全候補で作り、予測してから外す（学習・検証と同じ順）。
	MinTurnover float64
	models      []*Model
}

// manifest はマニフェストの JSON。
type manifest struct {
	Name         string   `json:"name"`
	Features     []string `json:"features"`
	Models       []string `json:"models"`
	PlanFeatures int      `json:"plan_features"`
	MinTurnover  float64  `json:"min_turnover"`
	// Note は人が読む説明（学習の条件・根拠のノート）。読み込みでは使わない。
	Note string `json:"note"`
}

// LoadSpec は path のモデルの束を読む。
func LoadSpec(path string) (*Spec, error) {
	if !strings.EqualFold(filepath.Ext(path), ".json") {
		m, err := Load(path)
		if err != nil {
			return nil, err
		}
		s := &Spec{Path: path, Name: filepath.Base(path), Features: FeatureNames, PlanFeatures: 1, models: []*Model{m}}
		return s, s.check()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mf manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(mf.Models) == 0 {
		return nil, fmt.Errorf("%s: models が空", path)
	}
	if mf.MinTurnover < 0 {
		return nil, fmt.Errorf("%s: min_turnover が負（%v）", path, mf.MinTurnover)
	}
	s := &Spec{Path: path, Name: mf.Name, Features: mf.Features, PlanFeatures: mf.PlanFeatures, MinTurnover: mf.MinTurnover}
	for _, rel := range mf.Models {
		p := rel
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), rel)
		}
		m, err := Load(p)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		s.models = append(s.models, m)
	}
	return s, s.check()
}

// check は特徴量の名前・モデルの入力の数・plan の版を確かめる。
func (s *Spec) check() error {
	if len(s.Features) == 0 {
		return fmt.Errorf("%s: features が空", s.Path)
	}
	seen := map[string]bool{}
	for _, name := range s.Features {
		if _, ok := featureFuncs[name]; !ok {
			return fmt.Errorf("%s: 知らない特徴量 %q（Go の rerank.featureFuncs に無い。bin を作り直す必要がある）", s.Path, name)
		}
		if seen[name] {
			return fmt.Errorf("%s: 特徴量 %q が重複", s.Path, name)
		}
		seen[name] = true
		if v := planFeaturesOf[name]; v > s.PlanFeatures {
			return fmt.Errorf("%s: 特徴量 %q は plan の版 %d が要るのに plan_features = %d", s.Path, name, v, s.PlanFeatures)
		}
	}
	if s.PlanFeatures < 1 {
		return fmt.Errorf("%s: plan_features は 1 以上", s.Path)
	}
	for i, m := range s.models {
		if m.NumFeatures != len(s.Features) {
			return fmt.Errorf("%s: モデル %d の入力は %d 個で、特徴量 %d 個と合いません", s.Path, i, m.NumFeatures, len(s.Features))
		}
	}
	return nil
}

// NumModels はモデルの本数（bagging の本数）。
func (s *Spec) NumModels() int { return len(s.models) }

// Scores はその日の候補の予測値（高いほど先に建てる）。rows は下限で外す前の全候補。
// 複数のモデルは予測を平均する。
func (s *Spec) Scores(rows []Input) []float64 {
	x := Ranked(RawNamed(rows, s.Features))
	out := make([]float64, len(x))
	for i := range x {
		sum := 0.0
		for _, m := range s.models {
			sum += m.Predict(x[i])
		}
		out[i] = sum / float64(len(s.models))
	}
	return out
}

var (
	specMu    sync.Mutex
	specCache = map[string]*Spec{}
)

// CachedSpec は path のモデルの束を 1 度だけ読む（バックテストは日ごとに並べ替えを呼ぶ）。
func CachedSpec(path string) (*Spec, error) {
	specMu.Lock()
	defer specMu.Unlock()
	if s, ok := specCache[path]; ok {
		return s, nil
	}
	s, err := LoadSpec(path)
	if err != nil {
		return nil, err
	}
	specCache[path] = s
	return s, nil
}
