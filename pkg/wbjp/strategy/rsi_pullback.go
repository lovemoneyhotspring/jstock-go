package strategy

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

// RSIPullback は長期上昇トレンド中の RSI(3) 押し目買い戦略。
type RSIPullback struct {
	FastSMA  int     // 50
	SlowSMA  int     // 200
	RSIPer   int     // 3
	RSIEntry float64 // 30.0
	RSIExit  float64 // 70.0
}

func NewRSIPullback() *RSIPullback {
	return &RSIPullback{
		FastSMA:  50,
		SlowSMA:  200,
		RSIPer:   3,
		RSIEntry: 30.0,
		RSIExit:  70.0,
	}
}

func (s *RSIPullback) Name() string {
	return "rsi_pullback"
}

func (s *RSIPullback) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	if n < s.SlowSMA+10 {
		return nil, nil
	}

	closes := make([]float64, n)
	for i, b := range bars {
		c, _ := b.Close.Float64()
		closes[i] = c
	}

	smaFast, err := indicators.SMA(closes, s.FastSMA)
	if err != nil {
		return nil, err
	}
	smaSlow, err := indicators.SMA(closes, s.SlowSMA)
	if err != nil {
		return nil, err
	}
	rsiVals, err := indicators.RSI(closes, s.RSIPer)
	if err != nil {
		return nil, err
	}

	idx := n - 1
	p := closes[idx]
	prevP := closes[idx-1]
	f := smaFast[idx]
	sl := smaSlow[idx]
	prevF := smaFast[idx-10]
	r := rsiVals[idx]
	prevR := rsiVals[idx-1]

	// 1. 長期上昇トレンド: 終値 > SMA200 かつ SMA50 > SMA200
	if p <= sl || f <= sl {
		return nil, nil
	}
	// 2. SMA50 が上向き
	if f <= prevF {
		return nil, nil
	}

	// 3. 手仕舞い判定 (RSI(3) >= 70)
	if r >= s.RSIExit {
		return &domain.Signal{
			Strategy:   s.Name(),
			Symbol:     symbol,
			Direction:  -1.0,
			Confidence: 0.8,
			Reason:     fmt.Sprintf("RSI(3)=%.1f 買われすぎ手仕舞い", r),
		}, nil
	}

	// 4. 押し目買い判定: 前日 RSI(3) < 30 かつ 当日陽線反発 (p > prevP)
	if prevR < s.RSIEntry && p > prevP {
		conf := (s.RSIEntry - prevR) / s.RSIEntry
		if conf < 0.3 {
			conf = 0.3
		}
		if conf > 1.0 {
			conf = 1.0
		}

		return &domain.Signal{
			Strategy:   s.Name(),
			Symbol:     symbol,
			Direction:  1.0,
			Confidence: conf,
			Reason:     fmt.Sprintf("上昇トレンド押し目反発 (前日RSI(3)=%.1f, 終値=%.0f)", prevR, p),
		}, nil
	}

	return nil, nil
}
