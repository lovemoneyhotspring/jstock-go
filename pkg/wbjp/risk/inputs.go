package risk

import (
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// RegimeInputFromBars は地合い判定に使う指数の直近値を足から組み立てる。
//
// 指標が揃わない（ウォームアップ中・足が無い）ものは nil のまま返す。
// RegimeExposure 側がそれを弱気として扱う。
func RegimeInputFromBars(bars []domain.Bar, cfg wbjpcfg.RegimeConfig) RegimeInput {
	var in RegimeInput
	if len(bars) == 0 {
		return in
	}
	last := bars[len(bars)-1].Close
	in.Close = &last

	closes := barCloses(bars)
	longMA, err := indicators.SMA(closes, cfg.SMALong)
	if err != nil {
		return in
	}
	midMA, err := indicators.SMA(closes, cfg.SMAMid)
	if err != nil {
		return in
	}

	longIdx := lastValidIndex(longMA)
	if longIdx >= 0 {
		d := decimal.NewFromFloat(longMA[longIdx])
		in.LongMA = &d
	}
	if midIdx := lastValidIndex(midMA); midIdx >= 0 {
		d := decimal.NewFromFloat(midMA[midIdx])
		in.MidMA = &d
	}

	// 長期線の傾き = 直近の長期線 − slope_lookback 本前の長期線。
	// 前の値がウォームアップ中（NaN）・範囲外なら傾きは出さない。
	// 負の lookback は設定では入らないが、入っても範囲外を読まない
	if longIdx >= 0 && cfg.SlopeLookback >= 0 {
		idx := longIdx - cfg.SlopeLookback
		if idx >= 0 && !math.IsNaN(longMA[idx]) {
			slope := in.LongMA.Sub(decimal.NewFromFloat(longMA[idx]))
			in.Slope = &slope
		}
	}
	return in
}

// TrendValue は残り玉（ランナー）を手仕舞う基準線の直近値。
//
// trend_exit_kind で線を選ぶ: sma / ema は終値の移動平均、donchian は当日を除く
// 過去 trend_exit_sma 本の最安値（終値がそれを割ったら手仕舞い）。
// trend_exit_sma が未設定・足が足りない・ウォームアップ中なら ok=false。
func TrendValue(bars []domain.Bar, stops wbjpcfg.StopsConfig) (value decimal.Decimal, ok bool) {
	if stops.TrendExitSMA == nil || *stops.TrendExitSMA <= 0 {
		return decimal.Zero, false
	}
	period := *stops.TrendExitSMA
	if len(bars) < period {
		return decimal.Zero, false
	}

	var series []float64
	var err error
	switch stops.TrendExitKind {
	case "ema":
		series, err = indicators.EMA(barCloses(bars), period)
	case "donchian":
		series, err = indicators.DonchianLow(barLows(bars), period)
	default:
		series, err = indicators.SMA(barCloses(bars), period)
	}
	if err != nil || len(series) == 0 {
		return decimal.Zero, false
	}
	last := series[len(series)-1]
	if math.IsNaN(last) {
		return decimal.Zero, false
	}
	return decimal.NewFromFloat(last), true
}

// barCloses は足の終値を float64 の並びにする。指標計算はこの形で受ける。
func barCloses(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i], _ = b.Close.Float64()
	}
	return out
}

func barLows(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i], _ = b.Low.Float64()
	}
	return out
}

// lastValidIndex は並びの末尾にある有効な値の位置。NaN しか無ければ -1。
func lastValidIndex(series []float64) int {
	for i := len(series) - 1; i >= 0; i-- {
		if !math.IsNaN(series[i]) {
			return i
		}
	}
	return -1
}
