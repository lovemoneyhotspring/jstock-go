package strategy

import (
	"fmt"
	"math"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// Combiner は複数戦略の意見を1本に合成する。
type Combiner func(symbol string, signals []domain.Signal, weights map[string]float64) domain.CombinedSignal

// CombineWeightedVote は重み付き平均合成。
func CombineWeightedVote(symbol string, signals []domain.Signal, weights map[string]float64) domain.CombinedSignal {
	if len(signals) == 0 {
		cs, _ := domain.NewCombinedSignal(symbol, 0.0, nil, "シグナルなし")
		return cs
	}

	totalWeight := 0.0
	weightedSum := 0.0
	contributions := make(map[string]float64)

	for _, s := range signals {
		w := 1.0
		if val, ok := weights[s.Strategy]; ok {
			w = val
		}
		if w <= 0 {
			continue
		}
		score := s.Score()
		weightedSum += score * w
		totalWeight += w
		contributions[s.Strategy] = score * w
	}

	combinedDir := 0.0
	if totalWeight > 0 {
		combinedDir = weightedSum / totalWeight
	}

	reason := fmt.Sprintf("重み付き投票: 合計スコア %g / 重み %g", weightedSum, totalWeight)
	cs, _ := domain.NewCombinedSignal(symbol, clamp(combinedDir), contributions, reason)
	return cs
}

func clamp(v float64) float64 {
	return math.Max(-1.0, math.Min(1.0, v))
}

func getContributions(signals []domain.Signal) map[string]float64 {
	m := make(map[string]float64)
	for _, s := range signals {
		m[s.Strategy] = s.Score()
	}
	return m
}

// CombineMajority は多数決合成。
func CombineMajority(symbol string, signals []domain.Signal, weights map[string]float64) domain.CombinedSignal {
	neutralBand := 0.05
	contributions := getContributions(signals)

	var bullish, bearish []domain.Signal
	for _, s := range signals {
		sc := s.Score()
		if sc > neutralBand {
			bullish = append(bullish, s)
		} else if sc < -neutralBand {
			bearish = append(bearish, s)
		}
	}

	if len(bullish) == len(bearish) {
		cs, _ := domain.NewCombinedSignal(symbol, 0.0, contributions, fmt.Sprintf("賛否同数 (強気%d/弱気%d)", len(bullish), len(bearish)))
		return cs
	}

	winners := bullish
	side := "強気"
	if len(bearish) > len(bullish) {
		winners = bearish
		side = "弱気"
	}

	totalWeight := 0.0
	weightedSum := 0.0
	for _, s := range winners {
		w := 1.0
		if val, ok := weights[s.Strategy]; ok {
			w = val
		}
		totalWeight += w
		weightedSum += s.Score() * w
	}

	direction := 0.0
	if totalWeight > 0 {
		direction = clamp(weightedSum / totalWeight)
	}

	cs, _ := domain.NewCombinedSignal(symbol, direction, contributions, fmt.Sprintf("%s多数 (強気%d/弱気%d)", side, len(bullish), len(bearish)))
	return cs
}

// CombineVeto は全員一致合成。
func CombineVeto(symbol string, signals []domain.Signal, weights map[string]float64) domain.CombinedSignal {
	neutralBand := 0.05
	contributions := getContributions(signals)

	var opinions []domain.Signal
	hasBull := false
	hasBear := false

	for _, s := range signals {
		sc := s.Score()
		if math.Abs(sc) > neutralBand {
			opinions = append(opinions, s)
			if sc > 0 {
				hasBull = true
			} else {
				hasBear = true
			}
		}
	}

	if len(opinions) == 0 {
		cs, _ := domain.NewCombinedSignal(symbol, 0.0, contributions, "意見なし")
		return cs
	}

	if hasBull && hasBear {
		cs, _ := domain.NewCombinedSignal(symbol, 0.0, contributions, "意見が割れたため見送り")
		return cs
	}

	totalWeight := 0.0
	weightedSum := 0.0
	for _, s := range opinions {
		w := 1.0
		if val, ok := weights[s.Strategy]; ok {
			w = val
		}
		totalWeight += w
		weightedSum += s.Score() * w
	}

	direction := 0.0
	if totalWeight > 0 {
		direction = clamp(weightedSum / totalWeight)
	}

	cs, _ := domain.NewCombinedSignal(symbol, direction, contributions, fmt.Sprintf("%d戦略が一致", len(opinions)))
	return cs
}

// CombinePriority は優先度（最大重み）優先合成。
func CombinePriority(symbol string, signals []domain.Signal, weights map[string]float64) domain.CombinedSignal {
	neutralBand := 0.05
	contributions := getContributions(signals)

	var opinions []domain.Signal
	for _, s := range signals {
		if math.Abs(s.Score()) > neutralBand {
			opinions = append(opinions, s)
		}
	}

	if len(opinions) == 0 {
		cs, _ := domain.NewCombinedSignal(symbol, 0.0, contributions, "意見なし")
		return cs
	}

	// 最大重み、重みタイなら戦略名のアルファベット順
	sort.Slice(opinions, func(i, j int) bool {
		wi := 1.0
		if v, ok := weights[opinions[i].Strategy]; ok {
			wi = v
		}
		wj := 1.0
		if v, ok := weights[opinions[j].Strategy]; ok {
			wj = v
		}
		if wi != wj {
			return wi > wj
		}
		return opinions[i].Strategy < opinions[j].Strategy
	})

	winner := opinions[0]
	cs, _ := domain.NewCombinedSignal(
		symbol,
		clamp(winner.Score()),
		contributions,
		fmt.Sprintf("最優先の %s を採用: %s", winner.Strategy, winner.Reason),
	)
	return cs
}

// GetCombinerByName は名前から合成器関数を返す。
func GetCombinerByName(name string) Combiner {
	switch name {
	case "majority":
		return CombineMajority
	case "veto":
		return CombineVeto
	case "priority":
		return CombinePriority
	case "weighted_vote":
		fallthrough
	default:
		return CombineWeightedVote
	}
}
