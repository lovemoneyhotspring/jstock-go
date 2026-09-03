package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

// TrendPullback 戦略: 上昇トレンド銘柄の押し目からの反発ブレイクアウト
type TrendPullback struct {
	TrendFast        int
	TrendSlow        int
	SlopeLookback    int
	HighLookback     int
	MaxPullbackPct   float64
	VolumeLookback   int
	VolumeDryupMax   float64
	BreakoutLookback int
	MinTurnover      float64
	MinATRRatio      float64
	MaxATRRatio      float64
}

func NewTrendPullback() *TrendPullback {
	return &TrendPullback{
		TrendFast:        50,
		TrendSlow:        200,
		SlopeLookback:    20,
		HighLookback:     20,
		MaxPullbackPct:   0.15,
		VolumeLookback:   20,
		VolumeDryupMax:   0.70,
		BreakoutLookback: 1,
		MinTurnover:      50_000_000,
		MinATRRatio:      0.01,
		MaxATRRatio:      0.08,
	}
}

func (t *TrendPullback) Name() string {
	return "trend_pullback"
}

func (t *TrendPullback) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	warmup := t.TrendSlow + t.SlopeLookback + 5
	if n < warmup {
		return nil, nil
	}

	closes := make([]float64, n)
	highs := make([]float64, n)
	lows := make([]float64, n)
	volumes := make([]float64, n)
	turnovers := make([]float64, n)

	for i, b := range bars {
		c, _ := b.Close.Float64()
		h, _ := b.High.Float64()
		l, _ := b.Low.Float64()
		v, _ := b.Volume.Float64()
		closes[i] = c
		highs[i] = h
		lows[i] = l
		volumes[i] = v
		turnovers[i] = c * v
	}

	smaFast, _ := indicators.SMA(closes, t.TrendFast)
	smaSlow, _ := indicators.SMA(closes, t.TrendSlow)
	atrVals, _ := indicators.ATR(highs, lows, closes, 14)
	volAvg, _ := indicators.SMA(volumes, t.VolumeLookback)
	turnoverAvg, _ := indicators.SMA(turnovers, 20)

	curr := n - 1
	prevSlope := curr - t.SlopeLookback

	closeCurr := closes[curr]
	fCurr := smaFast[curr]
	sCurr := smaSlow[curr]
	fPrev := smaFast[prevSlope]
	sPrev := smaSlow[prevSlope]

	// 1. 長期上昇トレンド: 終値 > SMA200 かつ SMA50 > SMA200
	if !(closeCurr > sCurr && fCurr > sCurr) {
		return nil, nil
	}

	// 2. 移動平均の傾き: SMA200 と SMA50 がともに上向き
	if !(sCurr > sPrev && fCurr > fPrev) {
		return nil, nil
	}

	// 3. 直近高値からの健全な押し目
	highestRecent := 0.0
	for i := curr - t.HighLookback; i < curr; i++ {
		if highs[i] > highestRecent {
			highestRecent = highs[i]
		}
	}
	if highestRecent <= 0 {
		return nil, nil
	}
	pullback := (highestRecent - closeCurr) / highestRecent
	if pullback > t.MaxPullbackPct {
		return nil, nil // 崩れすぎ
	}

	// 4. 出来高の枯渇（売り手が尽きたサイン）: 前日の出来高 <= 平均 * volume_dryup_max
	prevVol := volumes[curr-1]
	avgVol := volAvg[curr-1]
	if avgVol > 0 && prevVol > avgVol*t.VolumeDryupMax {
		return nil, nil
	}

	// 5. 反発ブレイクアウト: 終値 > 直近ブレイクアウト期間の高値
	breakoutHigh := 0.0
	for i := curr - t.BreakoutLookback; i < curr; i++ {
		if highs[i] > breakoutHigh {
			breakoutHigh = highs[i]
		}
	}
	if closeCurr <= breakoutHigh {
		return nil, nil
	}

	// 6. ATR 比率
	atrVal := atrVals[curr]
	atrRatio := atrVal / closeCurr
	if atrRatio < t.MinATRRatio || atrRatio > t.MaxATRRatio {
		return nil, nil
	}

	// 7. 売買代金下限
	if turnoverAvg[curr] < t.MinTurnover {
		return nil, nil
	}

	score := math.Min(1.0, 0.5+(1.0-pullback)*0.3+(1.0-prevVol/math.Max(1.0, avgVol))*0.2)
	reason := fmt.Sprintf("トレンド押し目反発: 押し目%.1f%%, 枯渇比率%.2f, 高値%.1fブレイク", pullback*100, prevVol/math.Max(1.0, avgVol), breakoutHigh)

	sig, err := domain.NewSignal(t.Name(), symbol, 1.0, score, reason, map[string]any{
		"pullback": pullback,
		"dryup":    prevVol / math.Max(1.0, avgVol),
		"atr":      atrVal,
	})
	return &sig, err
}
