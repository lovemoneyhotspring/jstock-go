package marketrules

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestTickSizeStandard(t *testing.T) {
	cases := []struct {
		price    string
		expected string
	}{
		{"1", "1"},
		{"1000", "1"},
		{"1000.5", "1"},
		{"3000", "1"},
		{"3001", "5"},
		{"5000", "5"},
		{"5001", "10"},
		{"30000", "10"},
		{"30001", "50"},
		{"50000", "50"},
		{"50001", "100"},
		{"300000", "100"},
		{"300001", "500"},
		{"500000", "500"},
		{"500001", "1000"},
		{"3000000", "1000"},
		{"3000001", "5000"},
		{"50000000", "50000"},
		{"50000001", "100000"},
	}

	for _, c := range cases {
		p := decimal.RequireFromString(c.price)
		exp := decimal.RequireFromString(c.expected)
		actual, err := TickSize(p, false)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", c.price, err)
		}
		if !actual.Equal(exp) {
			t.Errorf("TickSize(%s, false) = %s, want %s", c.price, actual, exp)
		}
	}
}

func TestTickSizeTOPIX500(t *testing.T) {
	cases := []struct {
		price    string
		expected string
	}{
		{"1", "0.1"},
		{"1000", "0.1"},
		{"1000.1", "0.5"},
		{"3000", "0.5"},
		{"3001", "1"},
		{"10000", "1"},
		{"10001", "5"},
		{"30000", "5"},
		{"30001", "10"},
		{"100000", "10"},
		{"100001", "50"},
		{"50000000", "10000"},
		{"50000001", "10000"},
	}

	for _, c := range cases {
		p := decimal.RequireFromString(c.price)
		exp := decimal.RequireFromString(c.expected)
		actual, err := TickSize(p, true)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", c.price, err)
		}
		if !actual.Equal(exp) {
			t.Errorf("TickSize(%s, true) = %s, want %s", c.price, actual, exp)
		}
	}
}

func TestSnapToTick(t *testing.T) {
	price := decimal.RequireFromString("3007") // 通常銘柄なら呼値5円
	buySnapped, err := SnapToTick(price, domain.SideBuy, false, RoundingConservative)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !buySnapped.Equal(decimal.RequireFromString("3005")) {
		t.Errorf("SnapToTick(buy, conservative) = %s, want 3005", buySnapped)
	}

	sellSnapped, err := SnapToTick(price, domain.SideSell, false, RoundingConservative)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sellSnapped.Equal(decimal.RequireFromString("3010")) {
		t.Errorf("SnapToTick(sell, conservative) = %s, want 3010", sellSnapped)
	}
}

func TestPriceLimits(t *testing.T) {
	cases := []struct {
		basePrice string
		expected  string
	}{
		{"99", "30"},
		{"100", "50"}, // 「未満」区分なので 100 は 200 未満（50）に入る
		{"199", "50"},
		{"200", "80"},
		{"499", "80"},
		{"500", "100"},
		{"700", "150"},
		{"1000", "300"},
		{"1500", "400"},
		{"2000", "500"},
		{"3000", "700"},
		{"5000", "1000"},
	}

	for _, c := range cases {
		bp := decimal.RequireFromString(c.basePrice)
		exp := decimal.RequireFromString(c.expected)
		w, err := PriceLimitWidth(bp)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", c.basePrice, err)
		}
		if !w.Equal(exp) {
			t.Errorf("PriceLimitWidth(%s) = %s, want %s", c.basePrice, w, exp)
		}
	}
}

func TestRoundToLot(t *testing.T) {
	lot := decimal.NewFromInt(100)
	cases := []struct {
		qty      string
		expected string
	}{
		{"0", "0"},
		{"50", "0"},
		{"100", "100"},
		{"150", "100"},
		{"299", "200"},
		{"-150", "-100"},
	}

	for _, c := range cases {
		q := decimal.RequireFromString(c.qty)
		exp := decimal.RequireFromString(c.expected)
		rounded, err := RoundToLot(q, lot)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !rounded.Equal(exp) {
			t.Errorf("RoundToLot(%s) = %s, want %s", c.qty, rounded, exp)
		}
	}
}

func TestIsTradingHours(t *testing.T) {
	// 2026-08-31 は月曜日
	// 9:15 JST -> true
	inSession := time.Date(2026, 8, 31, 9, 15, 0, 0, clock.Tokyo)
	if !IsTradingHours(inSession) {
		t.Errorf("9:15 JST on Monday should be trading hours")
	}

	// 12:00 JST (昼休み) -> false
	lunch := time.Date(2026, 8, 31, 12, 0, 0, 0, clock.Tokyo)
	if IsTradingHours(lunch) {
		t.Errorf("12:00 JST on Monday should NOT be trading hours")
	}

	// 15:35 JST (引け後) -> false
	afterClose := time.Date(2026, 8, 31, 15, 35, 0, 0, clock.Tokyo)
	if IsTradingHours(afterClose) {
		t.Errorf("15:35 JST on Monday should NOT be trading hours")
	}

	// 2026-08-30 は日曜日 -> false
	sunday := time.Date(2026, 8, 30, 10, 0, 0, 0, clock.Tokyo)
	if IsTradingHours(sunday) {
		t.Errorf("Sunday should NOT be trading hours")
	}
}
