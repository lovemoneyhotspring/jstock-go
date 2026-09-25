package universe

import (
	"math"
	"testing"
)

// oscZ は検証（test/dt_oscillator.py の rsi・test/dt_rsi_family.py のストキャス RSI）と
// 突き合わせるための終値（乱数の列を小数 6 桁に丸め、31 本目を 30 本目と同じ値にした）。
var oscZ = []float64{
	1000.024603, 1006.017547, 1000.516890, 982.853621, 973.956638, 954.830514, 955.979744, 981.950627, 972.331600, 960.340012,
	969.794547, 976.741451, 978.802873, 960.756414, 960.194501, 973.640300, 947.813395, 939.178284, 904.136977, 881.116733,
	849.251537, 845.267879, 824.110534, 828.593721, 831.195456, 828.093735, 787.443136, 779.004874, 778.249591, 780.015244,
	780.015244, 749.312173, 734.790409, 722.999518, 738.504008, 726.672457, 726.199958, 739.159107, 730.581790, 728.951464,
	730.563704, 731.496232, 713.791535, 714.879329, 734.573623, 712.191853, 724.538562, 726.270160, 717.012058, 746.280093,
	757.744444, 739.785593, 740.888936, 749.483664, 746.659217, 756.927204, 755.920898, 766.076236, 788.436713, 777.854039,
}

func TestOscillatorsMatchPandas(t *testing.T) {
	// 期待値は pandas（ewm(alpha=1/n, adjust=False, min_periods=n)、rolling(14) の min / max）
	cases := []struct {
		bars          int
		rsi2, stoch14 float64 // stoch14 が NaN なら nil を期待
	}{
		{20, 1.9268561924043655, math.NaN()},
		{27, 2.663575347644695, math.NaN()}, // RSI(14) が 14 本そろわない
		{28, 1.9455160762591415, 0.0},       // 前日が 14 日の最小
		{40, 34.282048230585424, 0.867663176436842},
		{60, 57.244070626167165, 0.8445907700215576},
	}
	for _, c := range cases {
		r2, s14 := Oscillators(oscZ[:c.bars])
		if r2 == nil || math.Abs(*r2-c.rsi2) > 1e-9 {
			t.Errorf("%d 本: RSI(2) = %v, want %v", c.bars, r2, c.rsi2)
		}
		if math.IsNaN(c.stoch14) {
			if s14 != nil {
				t.Errorf("%d 本: ストキャス RSI = %v, want nil", c.bars, *s14)
			}
			continue
		}
		if s14 == nil || math.Abs(*s14-c.stoch14) > 1e-9 {
			t.Errorf("%d 本: ストキャス RSI = %v, want %v", c.bars, s14, c.stoch14)
		}
	}
}

// 窓は直近 OscBars 本で切る（前夜の plan とバックテストが同じ本数で計算するため）。
func TestOscillatorsUseLastOscBars(t *testing.T) {
	long := make([]float64, 0, OscBars+80)
	for len(long) < OscBars+80 {
		long = append(long, oscZ...)
	}
	long = long[:OscBars+80]
	a2, a14 := Oscillators(long)
	b2, b14 := Oscillators(long[80:])
	if a2 == nil || b2 == nil || *a2 != *b2 || a14 == nil || b14 == nil || *a14 != *b14 {
		t.Fatalf("窓の外の足で値が変わった: %v/%v vs %v/%v", a2, a14, b2, b14)
	}
}

func TestOscillatorsFlatOrShort(t *testing.T) {
	if r2, s14 := Oscillators([]float64{100}); r2 != nil || s14 != nil {
		t.Fatalf("1 本で値が出た: %v %v", r2, s14)
	}
	flat := make([]float64, 40)
	for i := range flat {
		flat[i] = 500
	}
	// 上げも下げも無い列は RSI が定まらない（0 ÷ 0）
	if r2, s14 := Oscillators(flat); r2 != nil || s14 != nil {
		t.Fatalf("値動きの無い列で値が出た: %v %v", r2, s14)
	}
}
