package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

// ATRBreakout 戦略
type ATRBreakout struct {
	Channel     int
	ATRPeriod   int
	MinATRRatio float64
}

func NewATRBreakout(channel, atrPeriod int, minATRRatio float64) *ATRBreakout {
	if channel <= 0 {
		channel = 20
	}
	if atrPeriod <= 0 {
		atrPeriod = 14
	}
	if minATRRatio <= 0 {
		minATRRatio = 0.005
	}
	return &ATRBreakout{
		Channel:     channel,
		ATRPeriod:   atrPeriod,
		MinATRRatio: minATRRatio,
	}
}

func (a *ATRBreakout) Name() string {
	return "atr_breakout"
}

func (a *ATRBreakout) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	warmup := a.Channel
	if a.ATRPeriod > warmup {
		warmup = a.ATRPeriod
	}
	warmup += 2

	if n < warmup {
		return nil, nil
	}

	highs := make([]float64, n)
	lows := make([]float64, n)
	closes := make([]float64, n)

	for i, b := range bars {
		h, _ := b.High.Float64()
		l, _ := b.Low.Float64()
		c, _ := b.Close.Float64()
		highs[i] = h
		lows[i] = l
		closes[i] = c
	}

	donchianHigh, err := indicators.DonchianHigh(highs, a.Channel)
	if err != nil {
		return nil, err
	}
	donchianLow, err := indicators.DonchianLow(lows, a.Channel)
	if err != nil {
		return nil, err
	}
	atrVals, err := indicators.ATR(highs, lows, closes, a.ATRPeriod)
	if err != nil {
		return nil, err
	}

	lastIdx := n - 1
	upper := donchianHigh[lastIdx]
	lower := donchianLow[lastIdx]
	atrVal := atrVals[lastIdx]
	closePrice := closes[lastIdx]

	if math.IsNaN(upper) || math.IsNaN(lower) || math.IsNaN(atrVal) || closePrice <= 0 {
		return nil, nil
	}

	atrRatio := atrVal / closePrice
	if atrRatio < a.MinATRRatio {
		return nil, nil // 値動きが小さすぎるため見送り
	}

	confidence := math.Min(1.0, 0.4+atrRatio*20.0)
	latestHigh := highs[lastIdx]
	latestLow := lows[lastIdx]

	meta := map[string]any{
		"upper":     upper,
		"lower":     lower,
		"atr":       atrVal,
		"atr_ratio": atrRatio,
	}

	if latestHigh > upper {
		sig, err := domain.NewSignal(
			a.Name(),
			symbol,
			1.0,
			confidence,
			fmt.Sprintf("%d日高値 %.1f を上抜け", a.Channel, upper),
			meta,
		)
		return &sig, err
	}

	if latestLow < lower {
		sig, err := domain.NewSignal(
			a.Name(),
			symbol,
			-1.0,
			confidence,
			fmt.Sprintf("%d日安値 %.1f を下抜け", a.Channel, lower),
			meta,
		)
		return &sig, err
	}

	return nil, nil
}
