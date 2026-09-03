package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

type Strategy interface {
	Name() string
	OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error)
}

// SMACross 戦略
type SMACross struct {
	Fast int
	Slow int
}

func NewSMACross(fast, slow int) *SMACross {
	if fast <= 0 {
		fast = 25
	}
	if slow <= 0 {
		slow = 75
	}
	return &SMACross{Fast: fast, Slow: slow}
}

func (s *SMACross) Name() string {
	return "sma_cross"
}

func (s *SMACross) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	if n < s.Slow {
		return nil, nil
	}

	closes := make([]float64, n)
	for i, b := range bars {
		c, _ := b.Close.Float64()
		closes[i] = c
	}

	smaFast, _ := indicators.SMA(closes, s.Fast)
	smaSlow, _ := indicators.SMA(closes, s.Slow)

	currFast := smaFast[n-1]
	currSlow := smaSlow[n-1]
	prevFast := smaFast[n-2]
	prevSlow := smaSlow[n-2]

	direction := 0.0
	reason := "横ばい"

	if prevFast <= prevSlow && currFast > currSlow {
		direction = 1.0
		reason = fmt.Sprintf("ゴールデンクロス: SMA%d (%g) > SMA%d (%g)", s.Fast, currFast, s.Slow, currSlow)
	} else if prevFast >= prevSlow && currFast < currSlow {
		direction = -1.0
		reason = fmt.Sprintf("デッドクロス: SMA%d (%g) < SMA%d (%g)", s.Fast, currFast, s.Slow, currSlow)
	} else if currFast > currSlow {
		direction = 0.5
		reason = fmt.Sprintf("上昇トレンド継続: SMA%d > SMA%d", s.Fast, s.Slow)
	} else if currFast < currSlow {
		direction = -0.5
		reason = fmt.Sprintf("下降トレンド継続: SMA%d < SMA%d", s.Fast, s.Slow)
	}

	sig, err := domain.NewSignal(s.Name(), symbol, direction, 1.0, reason, nil)
	return &sig, err
}

// RSIReversion 戦略
type RSIReversion struct {
	Period     int
	Oversold   float64
	Overbought float64
	MaxADX     float64
}

func NewRSIReversion(period int, oversold, overbought, maxADX float64) *RSIReversion {
	if period <= 0 {
		period = 14
	}
	if oversold <= 0 {
		oversold = 30.0
	}
	if overbought <= 0 {
		overbought = 70.0
	}
	if maxADX <= 0 {
		maxADX = 40.0
	}
	return &RSIReversion{
		Period:     period,
		Oversold:   oversold,
		Overbought: overbought,
		MaxADX:     maxADX,
	}
}

func (r *RSIReversion) Name() string {
	return "rsi_reversion"
}

func (r *RSIReversion) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	if n < r.Period+1 {
		return nil, nil
	}

	closes := make([]float64, n)
	for i, b := range bars {
		c, _ := b.Close.Float64()
		closes[i] = c
	}

	rsiVals, err := indicators.RSI(closes, r.Period)
	if err != nil {
		return nil, err
	}
	currRSI := rsiVals[n-1]
	if math.IsNaN(currRSI) {
		return nil, nil
	}

	direction := 0.0
	reason := "中立"

	if currRSI <= r.Oversold {
		direction = 1.0
		reason = fmt.Sprintf("売られすぎ: RSI(%d) = %.1f <= %.1f", r.Period, currRSI, r.Oversold)
	} else if currRSI >= r.Overbought {
		direction = -1.0
		reason = fmt.Sprintf("買われすぎ: RSI(%d) = %.1f >= %.1f", r.Period, currRSI, r.Overbought)
	}

	sig, err := domain.NewSignal(r.Name(), symbol, direction, 1.0, reason, nil)
	return &sig, err
}

// Combiner は複数戦略の意見を1本に合成する。
type Combiner func(signals []domain.Signal, weights map[string]float64) domain.CombinedSignal

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
	cs, _ := domain.NewCombinedSignal(symbol, combinedDir, contributions, reason)
	return cs
}
