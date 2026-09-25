package universe

import "math"

// OscBars はオシレータ（RSI(2)・ストキャス RSI(14,14)）を計算する窓の本数（前日までの直近の足）。
//
// Wilder の平滑は窓の起点の影響が (1 − 1/n)^本数 で消える。RSI(14) で 120 本なら 1.4e-4 で、
// 全履歴で計算した検証（test/dt_rsi_family.py）と実質同じ値になる。前夜の plan とバックテストの
// パネルが同じ本数で計算するので、両者の値は一致する。
const OscBars = 120

// Oscillators は係数で揃えた終値の列（古い順、最後が前日）から、前日の引けまでの
// RSI(2) とストキャス RSI(14,14) を返す。計算できなければ nil。
//
// 定義は pandas の ewm(alpha=1/n, adjust=False, min_periods=n) と同じ（検証の test/dt_oscillator.py
// と test/dt_rsi_family.py）。比しか使わないので、終値の水準（係数の起点）には依らない。
func Oscillators(z []float64) (rsi2, stochRSI14 *float64) {
	if len(z) > OscBars {
		z = z[len(z)-OscBars:]
	}
	if r := rsiSeries(z, 2); len(r) > 0 && !math.IsNaN(r[len(r)-1]) {
		v := r[len(r)-1]
		rsi2 = &v
	}
	r14 := rsiSeries(z, 14)
	if len(r14) < 14 {
		return rsi2, nil
	}
	last := r14[len(r14)-14:]
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range last {
		if math.IsNaN(v) {
			return rsi2, nil
		}
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi > lo {
		v := (last[len(last)-1] - lo) / (hi - lo)
		stochRSI14 = &v
	}
	return rsi2, stochRSI14
}

// rsiSeries は終値の列の各時点の RSI(n)（Wilder の平滑）。値が n 本そろう前と、
// 上げ・下げがともに 0 の時点は NaN。返す列の長さは len(z)（最初の足は差分が無いので NaN）。
func rsiSeries(z []float64, n int) []float64 {
	out := make([]float64, len(z))
	if len(z) == 0 {
		return out
	}
	out[0] = math.NaN()
	alpha := 1 / float64(n)
	var up, dn float64
	count := 0
	for i := 1; i < len(z); i++ {
		d := z[i] - z[i-1]
		u, w := math.Max(d, 0), math.Max(-d, 0)
		if count == 0 {
			up, dn = u, w
		} else {
			up, dn = (1-alpha)*up+alpha*u, (1-alpha)*dn+alpha*w
		}
		count++
		if count < n || up+dn == 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = 100 * up / (up + dn)
	}
	return out
}
