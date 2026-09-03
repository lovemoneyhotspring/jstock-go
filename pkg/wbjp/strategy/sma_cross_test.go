package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func smaCrossContext(t *testing.T, closes []float64) *Context {
	t.Helper()
	return NewContext("", map[string][]domain.Bar{"AAA": barsFrom("AAA", closes, 1000, nil)}, nil, decimal.Zero)
}

func TestSMACrossGoldenCross(t *testing.T) {
	s, err := NewSMACross(5, 20)
	if err != nil {
		t.Fatal(err)
	}
	// 長く下げてから急騰させ、短期線に長期線を上抜けさせる。
	closes := make([]float64, 0, 60)
	for i := 0; i < 45; i++ {
		closes = append(closes, 100-float64(i)*0.5)
	}
	for i := 0; i < 15; i++ {
		closes = append(closes, closes[len(closes)-1]*1.05)
	}

	sigs, err := s.OnBars(smaCrossContext(t, closes))
	if err != nil {
		t.Fatal(err)
	}
	sig := mustSignal(t, sigs, "AAA")
	if sig.Direction <= 0 {
		t.Errorf("上昇局面では強気のはず: direction=%g reason=%s", sig.Direction, sig.Reason)
	}
}

func TestSMACrossHoldsOpinionBetweenCrosses(t *testing.T) {
	s, _ := NewSMACross(5, 20)
	// 単調上昇。クロスは起きないが「上で推移」の意見は出し続ける。
	// これが無いとクロス当日しか保有を維持できない。
	sigs, err := s.OnBars(smaCrossContext(t, growth(0.01, 60, 100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	sig := mustSignal(t, sigs, "AAA")
	if sig.Direction != 1.0 {
		t.Errorf("direction = %g, 期待 1.0", sig.Direction)
	}
	if !contains(sig.Reason, "上で推移") {
		t.Errorf("reason = %s", sig.Reason)
	}
}

func TestSMACrossSkipsShortHistory(t *testing.T) {
	s, _ := NewSMACross(5, 20)
	sigs, err := s.OnBars(smaCrossContext(t, growth(0.01, 10, 100, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if len(sigs) != 0 {
		t.Errorf("ウォームアップ不足の銘柄は黙って飛ばす: %v", sigs)
	}
}
