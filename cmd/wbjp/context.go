package main

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// closesOf は足の終値を float64 の並びにする。指標計算はこの形で受ける。
func closesOf(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		v, _ := b.Close.Float64()
		out[i] = v
	}
	return out
}

// trendValues は銘柄ごとのトレンド判定値（移動平均）を返す。
//
// 残り玉（ランナー）をどこで手仕舞うかの基準。trend_exit_sma が未設定なら
// 判定しないので空を返す。
func trendValues(barStore *data.BarStore, symbols []string, stops wbjpcfg.StopsConfig) map[string]decimal.Decimal {
	if stops.TrendExitSMA == nil || *stops.TrendExitSMA <= 0 {
		return nil
	}
	period := *stops.TrendExitSMA

	out := make(map[string]decimal.Decimal, len(symbols))
	for _, sym := range symbols {
		bars, err := barStore.Read(sym, "", "")
		if err != nil || len(bars) < period {
			continue
		}
		closes := closesOf(bars)

		var series []float64
		switch stops.TrendExitKind {
		case "ema":
			series, err = indicators.EMA(closes, period)
		default:
			series, err = indicators.SMA(closes, period)
		}
		if err != nil || len(series) == 0 {
			continue
		}
		last := series[len(series)-1]
		if last != last { // NaN（ウォームアップ中）
			continue
		}
		out[sym] = decimal.NewFromFloat(last)
	}
	return out
}

// regimeInput は地合い判定に使う指数の直近値を組み立てる。
//
// 指標が揃わない（ウォームアップ中・足が無い）ときは nil のまま返す。
// RegimeExposure 側がそれを弱気として扱う。
func regimeInput(barStore *data.BarStore, cfg wbjpcfg.RegimeConfig) risk.RegimeInput {
	var in risk.RegimeInput
	if cfg.Benchmark == "" {
		return in
	}

	bars, err := barStore.Read(cfg.Benchmark, "", "")
	if err != nil || len(bars) == 0 {
		return in
	}
	closes := closesOf(bars)

	last := bars[len(bars)-1].Close
	in.Close = &last

	longMA, err := indicators.SMA(closes, cfg.SMALong)
	if err != nil {
		return in
	}
	midMA, err := indicators.SMA(closes, cfg.SMAMid)
	if err != nil {
		return in
	}

	if v := lastValid(longMA); v != nil {
		d := decimal.NewFromFloat(*v)
		in.LongMA = &d
	}
	if v := lastValid(midMA); v != nil {
		d := decimal.NewFromFloat(*v)
		in.MidMA = &d
	}

	// 長期線の傾き = 直近の長期線 − slope_lookback 日前の長期線。
	idx := len(longMA) - 1 - cfg.SlopeLookback
	if in.LongMA != nil && idx >= 0 && longMA[idx] == longMA[idx] {
		slope := in.LongMA.Sub(decimal.NewFromFloat(longMA[idx]))
		in.Slope = &slope
	}
	return in
}

// lastValid は並びの末尾にある有効な値。NaN しか無ければ nil。
func lastValid(series []float64) *float64 {
	for i := len(series) - 1; i >= 0; i-- {
		if series[i] == series[i] {
			return &series[i]
		}
	}
	return nil
}

// quantitiesOf は建玉の数量だけを取り出す。
func quantitiesOf(positions map[string]domain.Position) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(positions))
	for sym, pos := range positions {
		out[sym] = pos.Quantity
	}
	return out
}
