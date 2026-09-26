package rerank

import (
	"fmt"
	"math"
	"slices"
)

// FeatureNames は従来の 1 本のモデル（signal.model に .txt を直に書く形）の入力の並び。
// test/dt_lgbm_train.py の FEATS と同じ。マニフェスト（Spec）はモデルごとに並びを持ち、
// ここに無い名前でも featureFuncs にあれば使える。
var FeatureNames = []string{
	"gap", "key", "vol20", "ret1", "ret5", "ret20", "pos20", "prev_intraday", "turn_cap",
	"log_turn", "log_cap", "log_price", "short_interest", "earn_yield", "n_cand", "rank_pct",
}

// Input は候補 1 銘柄の、順位化する前の材料。
type Input struct {
	// Gap は 9:01 の値段 ÷ 前日終値 − 1（丸める前）。
	Gap float64
	// Price は 9:01 の値段（気配・寄付）。
	Price float64
	// RuleRank は既存規則（gap_vol）での順位（1 始まり）。
	RuleRank int
	// TurnoverMed は売買代金 20 日中央値（円）、MktCap は前日の時価総額（百万円）。
	TurnoverMed float64
	MktCap      float64
	// 以下は取れなければ nil。
	Vol20         *float64
	Ret1          *float64
	RetD2         *float64
	Ret5          *float64
	Ret20         *float64
	Pos20         *float64
	PrevIntraday  *float64
	ShortInterest *float64
	EarnYield     *float64
}

// VolFloor は既存規則の鍵のボラの下限（selection.VolFloor と同じ値）。
const VolFloor = 0.02

// featureFuncs は名前ごとの、順位化する前の特徴量の式。取れない値は NaN。n はその日の候補数
// （帯とストップ安で絞った後）。学習側（test/dt_lgbm_train.py の raw_features）と同じ式にすること。
var featureFuncs = map[string]func(r Input, n float64) float64{
	"gap": func(r Input, _ float64) float64 { return r.Gap },
	"key": func(r Input, _ float64) float64 {
		if r.Vol20 == nil {
			return math.NaN()
		}
		// numpy の round（偶数丸め）に合わせる。学習側が np.round で作っている
		return math.RoundToEven(r.Gap*1e4) / 1e4 / math.Max(*r.Vol20, VolFloor)
	},
	"vol20":         func(r Input, _ float64) float64 { return orNaN(r.Vol20) },
	"ret1":          func(r Input, _ float64) float64 { return orNaN(r.Ret1) },
	"ret5":          func(r Input, _ float64) float64 { return orNaN(r.Ret5) },
	"ret20":         func(r Input, _ float64) float64 { return orNaN(r.Ret20) },
	"pos20":         func(r Input, _ float64) float64 { return orNaN(r.Pos20) },
	"prev_intraday": func(r Input, _ float64) float64 { return orNaN(r.PrevIntraday) },
	"turn_cap": func(r Input, _ float64) float64 {
		if r.MktCap > 0 {
			return r.TurnoverMed / r.MktCap
		}
		return math.NaN()
	},
	"log_turn": func(r Input, _ float64) float64 { return math.Log1p(r.TurnoverMed) },
	"log_cap": func(r Input, _ float64) float64 {
		if r.MktCap > 0 {
			return math.Log1p(r.MktCap)
		}
		return math.NaN()
	},
	"log_price": func(r Input, _ float64) float64 {
		if r.Price > 0 {
			return math.Log(r.Price)
		}
		return math.NaN()
	},
	"short_interest": func(r Input, _ float64) float64 { return orNaN(r.ShortInterest) },
	"earn_yield":     func(r Input, _ float64) float64 { return orNaN(r.EarnYield) },
	"n_cand":         func(_ Input, n float64) float64 { return n },
	"rank_pct":       func(r Input, n float64) float64 { return float64(r.RuleRank) / n },
	// ret_d2 は前々日の騰落、down2 は 2 日続落（前日・前々日とも下げ）なら 1、ほかは 0
	// （片方でも取れなければ 0。学習側の (ret1 < 0) & (ret_d2 < 0) が NaN を偽にするのと同じ）。
	"ret_d2": func(r Input, _ float64) float64 { return orNaN(r.RetD2) },
	"down2": func(r Input, _ float64) float64 {
		if r.Ret1 != nil && r.RetD2 != nil && *r.Ret1 < 0 && *r.RetD2 < 0 {
			return 1
		}
		return 0
	},
}

// planFeaturesOf は特徴量が要る plan の版（plan.RerankFeaturesVersion の番号）。
var planFeaturesOf = map[string]int{"ret_d2": 2, "down2": 2}

// Raw は候補の順位化する前の特徴量（行 × FeatureNames）。取れない値は NaN。
// rows はその日の候補すべて（帯とストップ安で絞った後）で、n_cand はその件数。
func Raw(rows []Input) [][]float64 {
	return RawNamed(rows, FeatureNames)
}

// RawNamed は names の並びで順位化する前の特徴量を作る。知らない名前は panic
// （Spec の読み込みで先に弾いているので、ふつうはここに来ない）。
func RawNamed(rows []Input, names []string) [][]float64 {
	fs := make([]func(Input, float64) float64, len(names))
	for j, name := range names {
		f, ok := featureFuncs[name]
		if !ok {
			panic(fmt.Sprintf("知らない特徴量 %q", name))
		}
		fs[j] = f
	}
	n := float64(len(rows))
	out := make([][]float64, len(rows))
	for i, r := range rows {
		out[i] = make([]float64, len(fs))
		for j, f := range fs {
			out[i][j] = f(r, n)
		}
	}
	return out
}

func orNaN(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}

// Ranked は列ごとにその日の百分位順位へ直す（pandas の rank(pct=True) と同じ:
// 同順位は平均順位、NaN は母数から外す）。NaN の欄は 0.5 で埋める。
func Ranked(raw [][]float64) [][]float64 {
	out := make([][]float64, len(raw))
	for i := range raw {
		out[i] = make([]float64, len(raw[i]))
	}
	if len(raw) == 0 {
		return out
	}
	idx := make([]int, 0, len(raw))
	for col := range raw[0] {
		idx = idx[:0]
		for i := range raw {
			if math.IsNaN(raw[i][col]) {
				out[i][col] = 0.5
				continue
			}
			idx = append(idx, i)
		}
		slices.SortStableFunc(idx, func(a, b int) int {
			x, y := raw[a][col], raw[b][col]
			switch {
			case x < y:
				return -1
			case x > y:
				return 1
			}
			return 0
		})
		count := float64(len(idx))
		for lo := 0; lo < len(idx); {
			hi := lo
			for hi+1 < len(idx) && raw[idx[hi+1]][col] == raw[idx[lo]][col] {
				hi++
			}
			// 1 始まりの順位 lo+1 … hi+1 の平均
			avg := float64(lo+hi+2) / 2
			for k := lo; k <= hi; k++ {
				out[idx[k]][col] = avg / count
			}
			lo = hi + 1
		}
	}
	return out
}

// Scores はその日の候補の予測値（高いほど先に建てる）。
func (m *Model) Scores(rows []Input) []float64 {
	x := Ranked(Raw(rows))
	out := make([]float64, len(x))
	for i := range x {
		out[i] = m.Predict(x[i])
	}
	return out
}
