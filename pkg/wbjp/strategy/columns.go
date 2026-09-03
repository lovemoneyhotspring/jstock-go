package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

// このファイルは View に「指標列」を生やす。列は名前でキャッシュされるので、
// 同じ期間の SMA を複数の戦略が要求しても計算は一度きり。
//
// 欠損は Python 版の None ではなく NaN で表す。比較演算が必ず false になる
// ので、条件式に紛れ込んでも「合格」にはならない。

func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// last は並びの末尾。空なら NaN。
func last(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	return xs[len(xs)-1]
}

// ago は末尾から k 本前の値。範囲外なら NaN。
func ago(xs []float64, k int) float64 {
	i := len(xs) - 1 - k
	if i < 0 || i >= len(xs) {
		return math.NaN()
	}
	return xs[i]
}

// at は範囲外を NaN にする添字アクセス。
func at(xs []float64, i int) float64 {
	if i < 0 || i >= len(xs) {
		return math.NaN()
	}
	return xs[i]
}

// anyNaN は 1 つでも欠損があるか。指標が揃う前の足を弾くのに使う。
func anyNaN(vs ...float64) bool {
	for _, v := range vs {
		if math.IsNaN(v) {
			return true
		}
	}
	return false
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

// shift は k 本ぶん後ろへずらす（先頭は NaN）。polars の shift(k) 相当。
func shift(xs []float64, k int) []float64 {
	out := nanSlice(len(xs))
	for i := k; i < len(xs); i++ {
		out[i] = xs[i-k]
	}
	return out
}

// rollingMean は当日を含む period 本の平均。窓が埋まるまで、または窓に
// 欠損が混じる間は NaN。
func rollingMean(xs []float64, period int) []float64 {
	out := nanSlice(len(xs))
	if period < 1 {
		return out
	}
	for i := range xs {
		if i+1 < period {
			continue
		}
		sum, ok := 0.0, true
		for j := i - period + 1; j <= i; j++ {
			if math.IsNaN(xs[j]) {
				ok = false
				break
			}
			sum += xs[j]
		}
		if ok {
			out[i] = sum / float64(period)
		}
	}
	return out
}

// rollingMax は当日を含む period 本の最大。
func rollingMax(xs []float64, period int) []float64 {
	out := nanSlice(len(xs))
	if period < 1 {
		return out
	}
	for i := range xs {
		if i+1 < period {
			continue
		}
		m := math.Inf(-1)
		ok := true
		for j := i - period + 1; j <= i; j++ {
			if math.IsNaN(xs[j]) {
				ok = false
				break
			}
			if xs[j] > m {
				m = xs[j]
			}
		}
		if ok {
			out[i] = m
		}
	}
	return out
}

// rollingStd は当日を含む period 本の標本標準偏差（ddof=1、polars の既定）。
func rollingStd(xs []float64, period int) []float64 {
	out := nanSlice(len(xs))
	if period < 2 {
		return out
	}
	for i := range xs {
		if i+1 < period {
			continue
		}
		sum, ok := 0.0, true
		for j := i - period + 1; j <= i; j++ {
			if math.IsNaN(xs[j]) {
				ok = false
				break
			}
			sum += xs[j]
		}
		if !ok {
			continue
		}
		mean := sum / float64(period)
		sq := 0.0
		for j := i - period + 1; j <= i; j++ {
			d := xs[j] - mean
			sq += d * d
		}
		out[i] = math.Sqrt(sq / float64(period-1))
	}
	return out
}

// -- View の指標列 ----------------------------------------------------------

// SMA は単純移動平均。
func (v View) SMA(period int) []float64 {
	return v.col(fmt.Sprintf("sma_%d", period), func(s *Series) []float64 {
		out, _ := indicators.SMA(s.Close, period)
		return out
	})
}

// EMA は指数移動平均（最初の period 本の単純平均を種にする）。
func (v View) EMA(period int) []float64 {
	return v.col(fmt.Sprintf("ema_%d", period), func(s *Series) []float64 {
		out, _ := indicators.EMA(s.Close, period)
		return out
	})
}

// RSI は Wilder の RSI。
func (v View) RSI(period int) []float64 {
	return v.col(fmt.Sprintf("rsi_%d", period), func(s *Series) []float64 {
		out, _ := indicators.RSI(s.Close, period)
		return out
	})
}

// ATR は平均実効変動幅。
func (v View) ATR(period int) []float64 {
	return v.col(fmt.Sprintf("atr_%d", period), func(s *Series) []float64 {
		out, _ := indicators.ATR(s.High, s.Low, s.Close, period)
		return out
	})
}

// ADX はトレンドの強さ。
func (v View) ADX(period int) []float64 {
	return v.col(fmt.Sprintf("adx_%d", period), func(s *Series) []float64 {
		res, err := indicators.ADX(s.High, s.Low, s.Close, period)
		if err != nil || res == nil {
			return nanSlice(len(s.Close))
		}
		return res.ADX
	})
}

// ROC は period 日の騰落率（%）。
func (v View) ROC(period int) []float64 {
	return v.col(fmt.Sprintf("roc_%d", period), func(s *Series) []float64 {
		out, _ := indicators.ROC(s.Close, period)
		return out
	})
}

// DonchianHigh は当日を除く過去 period 本の最高値。
//
// 当日を含めると当日高値が必ず上限に等しくなり、ブレイクが常に成立する
// 嘘のバックテスト結果になる。
func (v View) DonchianHigh(period int) []float64 {
	return v.col(fmt.Sprintf("donchian_high_%d", period), func(s *Series) []float64 {
		out, _ := indicators.DonchianHigh(s.High, period)
		return out
	})
}

// DonchianLow は当日を除く過去 period 本の最安値。
func (v View) DonchianLow(period int) []float64 {
	return v.col(fmt.Sprintf("donchian_low_%d", period), func(s *Series) []float64 {
		out, _ := indicators.DonchianLow(s.Low, period)
		return out
	})
}

// HighestHigh は当日を含む過去 period 本の最高値。
func (v View) HighestHigh(period int) []float64 {
	return v.col(fmt.Sprintf("high_%d", period), func(s *Series) []float64 {
		return rollingMax(s.High, period)
	})
}

// DollarVolume は period 日平均の売買代金（終値 × 出来高）。
func (v View) DollarVolume(period int) []float64 {
	return v.col(fmt.Sprintf("dollar_volume_%d", period), func(s *Series) []float64 {
		tv := make([]float64, len(s.Close))
		for i := range tv {
			tv[i] = s.Close[i] * s.Volume[i]
		}
		return rollingMean(tv, period)
	})
}

// PrevAvgVolume は当日を除く period 日平均の出来高。
//
// 当日の急増を分母に入れると相対出来高（RVOL）が薄まるので 1 本ずらす。
func (v View) PrevAvgVolume(period int) []float64 {
	return v.col(fmt.Sprintf("avg_volume_prev_%d", period), func(s *Series) []float64 {
		return rollingMean(shift(s.Volume, 1), period)
	})
}

// VolumeDryup は「前日の出来高 ÷ 前日までの平均出来高」。
//
// 売り手が尽きたかを測る。当日の出来高は反発で増えるため見ない。
func (v View) VolumeDryup(period int) []float64 {
	return v.col(fmt.Sprintf("volume_dryup_%d", period), func(s *Series) []float64 {
		avg := shift(rollingMean(s.Volume, period), 1)
		prev := shift(s.Volume, 1)
		out := nanSlice(len(prev))
		for i := range out {
			if !math.IsNaN(prev[i]) && !math.IsNaN(avg[i]) && avg[i] > 0 {
				out[i] = prev[i] / avg[i]
			}
		}
		return out
	})
}

// AnnualizedVol は日次対数リターンの標準偏差を年率換算したもの。
func (v View) AnnualizedVol(period int) []float64 {
	return v.col(fmt.Sprintf("vol_ann_%d", period), func(s *Series) []float64 {
		logRet := nanSlice(len(s.Close))
		for i := 1; i < len(s.Close); i++ {
			if s.Close[i-1] > 0 && s.Close[i] > 0 {
				logRet[i] = math.Log(s.Close[i] / s.Close[i-1])
			}
		}
		std := rollingStd(logRet, period)
		out := nanSlice(len(std))
		for i := range std {
			if !math.IsNaN(std[i]) {
				out[i] = std[i] * math.Sqrt(tradingDays)
			}
		}
		return out
	})
}

// Return は「skip 日前」を起点に lookback 日さかのぼった騰落率（比率）。
//
// skip=0 なら普通の lookback 日リターン。skip>0 は直近の平均回帰を
// 順位から取り除くために使う。
func (v View) Return(lookback, skip int) []float64 {
	return v.col(fmt.Sprintf("ret_%d_%d", lookback, skip), func(s *Series) []float64 {
		out := nanSlice(len(s.Close))
		for i := skip + lookback; i < len(s.Close); i++ {
			base := s.Close[i-skip-lookback]
			if base > 0 {
				out[i] = s.Close[i-skip]/base - 1.0
			}
		}
		return out
	})
}

// Gap は始値の前日終値比（比率）。
func (v View) Gap() []float64 {
	return v.col("gap", func(s *Series) []float64 {
		out := nanSlice(len(s.Open))
		for i := 1; i < len(s.Open); i++ {
			if s.Close[i-1] > 0 {
				out[i] = s.Open[i]/s.Close[i-1] - 1.0
			}
		}
		return out
	})
}

// RVOL は当日出来高 ÷ 前日までの平均出来高（相対出来高）。
func (v View) RVOL(period int) []float64 {
	return v.col(fmt.Sprintf("rvol_%d", period), func(s *Series) []float64 {
		avg := rollingMean(shift(s.Volume, 1), period)
		out := nanSlice(len(avg))
		for i := range out {
			if !math.IsNaN(avg[i]) && avg[i] > 0 {
				out[i] = s.Volume[i] / avg[i]
			}
		}
		return out
	})
}

// PrevClose / PrevHigh / PrevLow / PrevVolume は前日の値。
func (v View) PrevClose() []float64 {
	return v.col("prev_close", func(s *Series) []float64 { return shift(s.Close, 1) })
}
func (v View) PrevHigh() []float64 {
	return v.col("prev_high", func(s *Series) []float64 { return shift(s.High, 1) })
}
func (v View) PrevLow() []float64 {
	return v.col("prev_low", func(s *Series) []float64 { return shift(s.Low, 1) })
}
func (v View) PrevVolume() []float64 {
	return v.col("prev_volume", func(s *Series) []float64 { return shift(s.Volume, 1) })
}
