package main

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
)

var (
	appSettings   = settings.LoadAppSettings()
	configDirFlag string
)

func buildStrategies(stratCfg *wbjpcfg.StrategiesConfig) ([]strategy.Strategy, map[string]float64) {
	var strats []strategy.Strategy
	weights := make(map[string]float64)

	for _, se := range stratCfg.Strategies {
		if !se.IsEnabled() {
			continue
		}
		weights[se.Name] = se.Weight
		switch se.Name {
		case "sma_cross":
			strats = append(strats, strategy.NewSMACross(se.Fast, se.Slow))
		case "rsi_reversion":
			strats = append(strats, strategy.NewRSIReversion(se.Period, 30, 70, 40))
		case "atr_breakout":
			strats = append(strats, strategy.NewATRBreakout(20, 14, 0.005))
		case "trend_pullback":
			strats = append(strats, strategy.NewTrendPullback())
		case "rsi_pullback":
			strats = append(strats, strategy.NewRSIPullback())
		case "momentum_rank":
			strats = append(strats, strategy.NewMomentumRank())
		}
	}
	return strats, weights
}
