package indicators

import (
	"math"
	"testing"
)

var wilderCloses = []float64{
	44.34, 44.09, 44.15, 43.61, 44.33, 44.83, 45.10, 45.42, 45.84, 46.08,
	45.89, 46.03, 45.61, 46.28, 46.28, 46.00, 46.03, 46.41, 46.22, 45.64,
}

func almostEqual(a, b, tol float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	return math.Abs(a-b) <= tol
}

func TestSMA(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50}
	sma, err := SMA(vals, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !math.IsNaN(sma[0]) || !math.IsNaN(sma[1]) {
		t.Errorf("expected NaN for first 2 elements, got %f, %f", sma[0], sma[1])
	}
	if !almostEqual(sma[2], 20.0, 1e-6) {
		t.Errorf("sma[2] = %f, want 20.0", sma[2])
	}
	if !almostEqual(sma[3], 30.0, 1e-6) {
		t.Errorf("sma[3] = %f, want 30.0", sma[3])
	}
	if !almostEqual(sma[4], 40.0, 1e-6) {
		t.Errorf("sma[4] = %f, want 40.0", sma[4])
	}
}

func TestRSIWilder(t *testing.T) {
	// period = 14 での Wilder 氏の検証
	period := 14
	rsi, err := RSI(wilderCloses, period)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 14本目（index 14）で最初の有効値が出る
	if math.IsNaN(rsi[14]) {
		t.Fatalf("rsi[14] should not be NaN")
	}
	// RSI は 0〜100 の範囲内であること
	for i := 14; i < len(rsi); i++ {
		if rsi[i] < 0 || rsi[i] > 100 {
			t.Errorf("rsi[%d] = %f out of range [0, 100]", i, rsi[i])
		}
	}

	// 終値 46.28 の時点での RSI はおおよそ 70 前後
	if rsi[14] < 60 || rsi[14] > 80 {
		t.Errorf("rsi[14] = %f, expected around 70", rsi[14])
	}
}

func TestBollingerBands(t *testing.T) {
	bb, err := BollingerBands(wilderCloses, 10, 2.0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i := 9; i < len(wilderCloses); i++ {
		if bb.Upper[i] < bb.Mid[i] || bb.Mid[i] < bb.Lower[i] {
			t.Errorf("bb invalid order at %d: upper=%f, mid=%f, lower=%f", i, bb.Upper[i], bb.Mid[i], bb.Lower[i])
		}
	}
}

func TestTrueRangeAndATR(t *testing.T) {
	high := make([]float64, len(wilderCloses))
	low := make([]float64, len(wilderCloses))
	for i, c := range wilderCloses {
		high[i] = c + 0.5
		low[i] = c - 0.5
	}

	tr, err := TrueRange(high, low, wilderCloses)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, v := range tr {
		if v < 1.0 { // high - low = 1.0 なので最低でも 1.0
			t.Errorf("tr[%d] = %f should be >= 1.0", i, v)
		}
	}

	atr, err := ATR(high, low, wilderCloses, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if math.IsNaN(atr[4]) {
		t.Errorf("atr[4] should not be NaN")
	}
}

func TestDonchian(t *testing.T) {
	high := []float64{10, 20, 15, 30, 25, 40}
	low := []float64{5, 12, 10, 18, 14, 22}

	dh, err := DonchianHigh(high, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 当日を除く過去3本: index 3 は high[0..3] = {10, 20, 15} の max = 20
	if !almostEqual(dh[3], 20.0, 1e-6) {
		t.Errorf("dh[3] = %f, want 20.0", dh[3])
	}
	// index 4 は high[1..4] = {20, 15, 30} の max = 30
	if !almostEqual(dh[4], 30.0, 1e-6) {
		t.Errorf("dh[4] = %f, want 30.0", dh[4])
	}

	dl, err := DonchianLow(low, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// index 3 は low[0..3] = {5, 12, 10} の min = 5
	if !almostEqual(dl[3], 5.0, 1e-6) {
		t.Errorf("dl[3] = %f, want 5.0", dl[3])
	}
}

func TestMACD(t *testing.T) {
	res, err := MACD(wilderCloses, 5, 10, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.MACD) != len(wilderCloses) {
		t.Fatalf("unexpected length: %d", len(res.MACD))
	}
}
