// Package rerank はロングの候補を機械学習（LightGBM）で並べ替える。
//
// 候補の母集団・ギャップの帯・N 本の取り方は既存規則（selection）のまま。変わるのは
// 並べる順番だけで、既存規則の鍵（gap_vol）と順位も特徴量に入る。
//
// モデルは LightGBM のテキスト形式（booster.save_model）を読み、ここで木をたどる。
// 学習は test/dt_lgbm_train.py。根拠は vault の
// 20-research/2026-09-jp-daytrade-ml-open-quote.md。
package rerank

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Model は回帰の木の集まり。予測は葉の値の合計（学習率は葉の値に掛かっている）。
type Model struct {
	// NumFeatures はモデルの入力の数（max_feature_idx + 1）。
	NumFeatures int
	trees       []tree
}

type tree struct {
	feature   []int
	threshold []float64
	// decision は LightGBM の decision_type（bit0: カテゴリ、bit1: 欠損は左、bit2-3: 欠損の種類）。
	decision []int
	left     []int
	right    []int
	leaf     []float64
}

// 欠損の種類（decision_type の bit2-3）。
const (
	missingNone = 0
	missingZero = 1
	missingNaN  = 2
)

// zeroThreshold は LightGBM の kZeroThreshold（これより小さい絶対値を 0 とみなす）。
const zeroThreshold = 1e-35

// Load はモデルファイルを読む。カテゴリの分岐・線形の木は使っていないので、あれば誤りにする。
func Load(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("並べ替えのモデルを開けません: %w", err)
	}
	defer f.Close()

	m := &Model{}
	var cur map[string]string
	flush := func() error {
		if cur == nil {
			return nil
		}
		t, err := parseTree(cur)
		if err != nil {
			return fmt.Errorf("%s の木 %d: %w", path, len(m.trees), err)
		}
		m.trees = append(m.trees, t)
		cur = nil
		return nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	header := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "end of trees":
			if err := flush(); err != nil {
				return nil, err
			}
			header = false
		case strings.HasPrefix(line, "Tree="):
			if err := flush(); err != nil {
				return nil, err
			}
			cur = map[string]string{}
			header = false
		case line == "" || !strings.Contains(line, "="):
		default:
			key, value, _ := strings.Cut(line, "=")
			if cur != nil {
				cur[key] = value
				continue
			}
			if !header {
				continue
			}
			switch key {
			case "max_feature_idx":
				n, err := strconv.Atoi(value)
				if err != nil {
					return nil, fmt.Errorf("%s の max_feature_idx を読めません: %w", path, err)
				}
				m.NumFeatures = n + 1
			case "num_class", "num_tree_per_iteration":
				if value != "1" {
					return nil, fmt.Errorf("%s は多クラスのモデル（%s=%s）で使えません", path, key, value)
				}
			case "objective":
				if !strings.HasPrefix(value, "regression") {
					return nil, fmt.Errorf("%s の目的関数 %q は使えません（回帰だけ）", path, value)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if len(m.trees) == 0 || m.NumFeatures == 0 {
		return nil, fmt.Errorf("%s に木がありません", path)
	}
	for i := range m.trees {
		if err := m.trees[i].validate(m.NumFeatures); err != nil {
			return nil, fmt.Errorf("%s の木 %d: %w", path, i, err)
		}
	}
	return m, nil
}

// validate は節の番号を確かめる。壊れたモデルで predict が範囲の外を読んだり、子が親へ戻って
// 回り続けたりしないようにする（open が止まるとロックを握ったままになり、close まで見送られる）。
// LightGBM は分岐のたびに節を末尾へ足すので、子の節の番号は必ず親より大きい。
func (t *tree) validate(numFeatures int) error {
	for i := range t.feature {
		if t.feature[i] < 0 || t.feature[i] >= numFeatures {
			return fmt.Errorf("節 %d の特徴量の番号 %d が範囲の外（0〜%d）", i, t.feature[i], numFeatures-1)
		}
		for _, child := range []int{t.left[i], t.right[i]} {
			if child >= 0 && (child <= i || child >= len(t.feature)) {
				return fmt.Errorf("節 %d の子 %d が範囲の外（%d〜%d）", i, child, i+1, len(t.feature)-1)
			}
			if child < 0 && ^child >= len(t.leaf) {
				return fmt.Errorf("節 %d の葉 %d が範囲の外（0〜%d）", i, ^child, len(t.leaf)-1)
			}
		}
	}
	return nil
}

func parseTree(kv map[string]string) (tree, error) {
	var t tree
	if kv["is_linear"] == "1" {
		return t, fmt.Errorf("線形の木は使えません")
	}
	if n, _ := strconv.Atoi(kv["num_cat"]); n > 0 {
		return t, fmt.Errorf("カテゴリの分岐は使えません")
	}
	var err error
	if t.leaf, err = floats(kv["leaf_value"]); err != nil {
		return t, fmt.Errorf("leaf_value: %w", err)
	}
	leaves, _ := strconv.Atoi(kv["num_leaves"])
	if leaves != len(t.leaf) || leaves == 0 {
		return t, fmt.Errorf("葉の数が合いません（num_leaves=%d、leaf_value %d 個）", leaves, len(t.leaf))
	}
	if leaves == 1 {
		return t, nil
	}
	if t.feature, err = ints(kv["split_feature"]); err != nil {
		return t, fmt.Errorf("split_feature: %w", err)
	}
	if t.threshold, err = floats(kv["threshold"]); err != nil {
		return t, fmt.Errorf("threshold: %w", err)
	}
	if t.decision, err = ints(kv["decision_type"]); err != nil {
		return t, fmt.Errorf("decision_type: %w", err)
	}
	if t.left, err = ints(kv["left_child"]); err != nil {
		return t, fmt.Errorf("left_child: %w", err)
	}
	if t.right, err = ints(kv["right_child"]); err != nil {
		return t, fmt.Errorf("right_child: %w", err)
	}
	nodes := leaves - 1
	for _, n := range []int{len(t.feature), len(t.threshold), len(t.decision), len(t.left), len(t.right)} {
		if n != nodes {
			return t, fmt.Errorf("節の数が合いません（%d 個のはずが %d 個）", nodes, n)
		}
	}
	for _, d := range t.decision {
		if d&1 != 0 {
			return t, fmt.Errorf("カテゴリの分岐は使えません")
		}
	}
	return t, nil
}

func floats(s string) ([]float64, error) {
	fields := strings.Fields(s)
	out := make([]float64, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func ints(s string) ([]int, error) {
	fields := strings.Fields(s)
	out := make([]int, len(fields))
	for i, f := range fields {
		v, err := strconv.Atoi(f)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Predict は 1 行の予測値。x の長さは NumFeatures。
func (m *Model) Predict(x []float64) float64 {
	sum := 0.0
	for i := range m.trees {
		sum += m.trees[i].predict(x)
	}
	return sum
}

// predict は LightGBM の Tree::NumericalDecision と同じ規則で葉までたどる。
func (t *tree) predict(x []float64) float64 {
	if len(t.feature) == 0 {
		return t.leaf[0]
	}
	node := 0
	for node >= 0 {
		v := x[t.feature[node]]
		d := t.decision[node]
		missing := (d >> 2) & 3
		if math.IsNaN(v) && missing != missingNaN {
			v = 0
		}
		goLeft := v <= t.threshold[node]
		if (missing == missingZero && math.Abs(v) <= zeroThreshold) || (missing == missingNaN && math.IsNaN(v)) {
			goLeft = d&2 != 0
		}
		if goLeft {
			node = t.left[node]
		} else {
			node = t.right[node]
		}
	}
	return t.leaf[^node]
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*Model{}
)

// Cached は path のモデルを 1 度だけ読む（バックテストは日ごとに並べ替えを呼ぶ）。
func Cached(path string) (*Model, error) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if m, ok := cache[path]; ok {
		return m, nil
	}
	m, err := Load(path)
	if err != nil {
		return nil, err
	}
	if m.NumFeatures != len(FeatureNames) {
		return nil, fmt.Errorf("%s の入力は %d 個で、特徴量 %d 個と合いません", path, m.NumFeatures, len(FeatureNames))
	}
	cache[path] = m
	return m, nil
}
