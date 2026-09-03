package indicators

import (
	"fmt"
	"math"
)

// SMA は単純移動平均を計算する。
func SMA(values []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(values)
	res := make([]float64, n)
	for i := range res {
		res[i] = math.NaN()
	}
	if n < period {
		return res, nil
	}

	sum := 0.0
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	res[period-1] = sum / float64(period)

	for i := period; i < n; i++ {
		sum += values[i] - values[i-period]
		res[i] = sum / float64(period)
	}
	return res, nil
}

// SeededEWM は最初の period 個の単純平均を初期種とする指数移動平均を計算する。
// values 内に NaN がある場合はスキップし、有効値が period 個揃った時点でシードする。
func SeededEWM(values []float64, period int, alpha float64) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(values)
	res := make([]float64, n)
	for i := range res {
		res[i] = math.NaN()
	}

	// 最初の有効値 period 個を探す
	validCount := 0
	sum := 0.0
	seedIdx := -1
	for i := 0; i < n; i++ {
		if !math.IsNaN(values[i]) {
			sum += values[i]
			validCount++
			if validCount == period {
				seedIdx = i
				break
			}
		}
	}

	if seedIdx == -1 {
		return res, nil
	}

	// 初期種を格納
	prev := sum / float64(period)
	res[seedIdx] = prev

	for i := seedIdx + 1; i < n; i++ {
		val := values[i]
		if math.IsNaN(val) {
			res[i] = prev
			continue
		}
		curr := alpha*val + (1.0-alpha)*prev
		res[i] = curr
		prev = curr
	}
	return res, nil
}

// EMA は指数移動平均（α = 2 / (period + 1)）を計算する。
func EMA(values []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	alpha := 2.0 / float64(period+1)
	return SeededEWM(values, period, alpha)
}

// WilderEMA は Wilder の平滑化（α = 1 / period）を計算する。
func WilderEMA(values []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	alpha := 1.0 / float64(period)
	return SeededEWM(values, period, alpha)
}

// TrueRange は真の変動幅（TR）を計算する。
func TrueRange(high, low, close []float64) ([]float64, error) {
	n := len(high)
	if len(low) != n || len(close) != n {
		return nil, fmt.Errorf("スライスの長さが一致しません: high=%d, low=%d, close=%d", n, len(low), len(close))
	}
	tr := make([]float64, n)
	if n == 0 {
		return tr, nil
	}

	tr[0] = high[0] - low[0]
	for i := 1; i < n; i++ {
		hl := high[i] - low[i]
		hc := math.Abs(high[i] - close[i-1])
		lc := math.Abs(low[i] - close[i-1])

		maxVal := hl
		if hc > maxVal {
			maxVal = hc
		}
		if lc > maxVal {
			maxVal = lc
		}
		tr[i] = maxVal
	}
	return tr, nil
}

// ATR は平均真の変動幅（ATR）を計算する。
func ATR(high, low, close []float64, period int) ([]float64, error) {
	tr, err := TrueRange(high, low, close)
	if err != nil {
		return nil, err
	}
	return WilderEMA(tr, period)
}

// RSI は相対力指数（0〜100）を計算する。
func RSI(close []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(close)
	rsi := make([]float64, n)
	for i := range rsi {
		rsi[i] = math.NaN()
	}
	if n < 2 {
		return rsi, nil
	}

	gain := make([]float64, n)
	loss := make([]float64, n)
	gain[0] = math.NaN()
	loss[0] = math.NaN()

	for i := 1; i < n; i++ {
		delta := close[i] - close[i-1]
		if delta > 0 {
			gain[i] = delta
			loss[i] = 0.0
		} else if delta < 0 {
			gain[i] = 0.0
			loss[i] = -delta
		} else {
			gain[i] = 0.0
			loss[i] = 0.0
		}
	}

	avgGain, err := WilderEMA(gain, period)
	if err != nil {
		return nil, err
	}
	avgLoss, err := WilderEMA(loss, period)
	if err != nil {
		return nil, err
	}

	for i := 0; i < n; i++ {
		g := avgGain[i]
		l := avgLoss[i]
		if math.IsNaN(g) || math.IsNaN(l) {
			continue
		}
		if l == 0.0 {
			rsi[i] = 100.0
		} else {
			rs := g / l
			rsi[i] = 100.0 - (100.0 / (1.0 + rs))
		}
	}
	return rsi, nil
}

// ROC は価格変化率（%）を計算する。
func ROC(close []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(close)
	res := make([]float64, n)
	for i := range res {
		res[i] = math.NaN()
	}
	for i := period; i < n; i++ {
		prev := close[i-period]
		if prev != 0 {
			res[i] = ((close[i] / prev) - 1.0) * 100.0
		}
	}
	return res, nil
}

// BollingerBandsResult はボリンジャーバンドの計算結果。
type BollingerBandsResult struct {
	Mid   []float64
	Upper []float64
	Lower []float64
}

// BollingerBands はボリンジャーバンド（ddof=0 母集団標準偏差）を計算する。
func BollingerBands(close []float64, period int, numStd float64) (*BollingerBandsResult, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(close)
	mid, err := SMA(close, period)
	if err != nil {
		return nil, err
	}

	upper := make([]float64, n)
	lower := make([]float64, n)
	for i := range upper {
		upper[i] = math.NaN()
		lower[i] = math.NaN()
	}

	for i := period - 1; i < n; i++ {
		m := mid[i]
		var sumSq float64
		for j := i - period + 1; j <= i; j++ {
			diff := close[j] - m
			sumSq += diff * diff
		}
		std := math.Sqrt(sumSq / float64(period)) // ddof=0
		upper[i] = m + numStd*std
		lower[i] = m - numStd*std
	}

	return &BollingerBandsResult{
		Mid:   mid,
		Upper: upper,
		Lower: lower,
	}, nil
}

// MACDResult は MACD の計算結果。
type MACDResult struct {
	MACD   []float64
	Signal []float64
	Hist   []float64
}

// MACD は MACD (fast, slow, signal) を計算する。
func MACD(close []float64, fast, slow, signal int) (*MACDResult, error) {
	if fast >= slow {
		return nil, fmt.Errorf("fast は slow より小さく: fast=%d, slow=%d", fast, slow)
	}
	fastEMA, err := EMA(close, fast)
	if err != nil {
		return nil, err
	}
	slowEMA, err := EMA(close, slow)
	if err != nil {
		return nil, err
	}

	n := len(close)
	macdLine := make([]float64, n)
	for i := 0; i < n; i++ {
		if math.IsNaN(fastEMA[i]) || math.IsNaN(slowEMA[i]) {
			macdLine[i] = math.NaN()
		} else {
			macdLine[i] = fastEMA[i] - slowEMA[i]
		}
	}

	signalLine, err := EMA(macdLine, signal)
	if err != nil {
		return nil, err
	}

	hist := make([]float64, n)
	for i := 0; i < n; i++ {
		if math.IsNaN(macdLine[i]) || math.IsNaN(signalLine[i]) {
			hist[i] = math.NaN()
		} else {
			hist[i] = macdLine[i] - signalLine[i]
		}
	}

	return &MACDResult{
		MACD:   macdLine,
		Signal: signalLine,
		Hist:   hist,
	}, nil
}

// DonchianHigh は過去 period 本の最高値（当日を除く）を計算する。
func DonchianHigh(high []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(high)
	res := make([]float64, n)
	for i := range res {
		res[i] = math.NaN()
	}

	for i := period; i < n; i++ {
		maxVal := -math.MaxFloat64
		for j := i - period; j < i; j++ {
			if high[j] > maxVal {
				maxVal = high[j]
			}
		}
		res[i] = maxVal
	}
	return res, nil
}

// DonchianLow は過去 period 本の最安値（当日を除く）を計算する。
func DonchianLow(low []float64, period int) ([]float64, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(low)
	res := make([]float64, n)
	for i := range res {
		res[i] = math.NaN()
	}

	for i := period; i < n; i++ {
		minVal := math.MaxFloat64
		for j := i - period; j < i; j++ {
			if low[j] < minVal {
				minVal = low[j]
			}
		}
		res[i] = minVal
	}
	return res, nil
}

// ADXResult は ADX と ±DI の計算結果。
type ADXResult struct {
	ADX     []float64
	DIPlus  []float64
	DIMinus []float64
}

// ADX は ADX と ±DI を計算する。
func ADX(high, low, close []float64, period int) (*ADXResult, error) {
	if period < 1 {
		return nil, fmt.Errorf("period は 1 以上: %d", period)
	}
	n := len(high)
	if len(low) != n || len(close) != n {
		return nil, fmt.Errorf("スライスの長さが一致しません")
	}

	plusDM := make([]float64, n)
	minusDM := make([]float64, n)
	plusDM[0] = math.NaN()
	minusDM[0] = math.NaN()

	for i := 1; i < n; i++ {
		upMove := high[i] - high[i-1]
		downMove := low[i-1] - low[i]

		if upMove > downMove && upMove > 0 {
			plusDM[i] = upMove
		} else {
			plusDM[i] = 0.0
		}

		if downMove > upMove && downMove > 0 {
			minusDM[i] = downMove
		} else {
			minusDM[i] = 0.0
		}
	}

	tr, err := TrueRange(high, low, close)
	if err != nil {
		return nil, err
	}
	smoothedTR, err := WilderEMA(tr, period)
	if err != nil {
		return nil, err
	}
	smoothedPlusDM, err := WilderEMA(plusDM, period)
	if err != nil {
		return nil, err
	}
	smoothedMinusDM, err := WilderEMA(minusDM, period)
	if err != nil {
		return nil, err
	}

	diPlus := make([]float64, n)
	diMinus := make([]float64, n)
	dx := make([]float64, n)
	for i := range diPlus {
		diPlus[i] = math.NaN()
		diMinus[i] = math.NaN()
		dx[i] = math.NaN()
	}

	for i := 0; i < n; i++ {
		sTR := smoothedTR[i]
		if math.IsNaN(sTR) || sTR == 0 {
			continue
		}
		pDM := smoothedPlusDM[i]
		mDM := smoothedMinusDM[i]
		if math.IsNaN(pDM) || math.IsNaN(mDM) {
			continue
		}

		dip := 100.0 * pDM / sTR
		dim := 100.0 * mDM / sTR
		diPlus[i] = dip
		diMinus[i] = dim

		diSum := dip + dim
		if diSum == 0 {
			dx[i] = 0.0
		} else {
			dx[i] = 100.0 * math.Abs(dip-dim) / diSum
		}
	}

	adx, err := WilderEMA(dx, period)
	if err != nil {
		return nil, err
	}

	return &ADXResult{
		ADX:     adx,
		DIPlus:  diPlus,
		DIMinus: diMinus,
	}, nil
}
