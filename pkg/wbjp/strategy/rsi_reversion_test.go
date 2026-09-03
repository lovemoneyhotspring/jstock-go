package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestRSIReversionBuysOversold(t *testing.T) {
	s, err := NewRSIReversion(14, 30, 70, 14, 100) // ADX 上限を実質無効にして RSI だけ見る
	if err != nil {
		t.Fatal(err)
	}
	// 横ばいのあと連続陰線で売られすぎにする。
	closes := make([]float64, 0, 80)
	for i := 0; i < 60; i++ {
		closes = append(closes, 100)
	}
	for i := 0; i < 20; i++ {
		closes = append(closes, closes[len(closes)-1]*0.98)
	}
	ctx := NewContext("", map[string][]domain.Bar{"AAA": barsFrom("AAA", closes, 1000, nil)}, nil, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != 1.0 {
		t.Errorf("売られすぎでは買い: direction=%g reason=%s", sig.Direction, sig.Reason)
	}
}

// 強いトレンドが出ている間は逆張りしない。逆張りが最も損をする局面なので、
// 黙る判断が効いているかを確かめる。
func TestRSIReversionStaysSilentInStrongTrend(t *testing.T) {
	s, _ := NewRSIReversion(14, 30, 70, 14, 15) // ADX 上限を低くして必ず黙らせる
	closes := make([]float64, 0, 80)
	for i := 0; i < 60; i++ {
		closes = append(closes, 100)
	}
	for i := 0; i < 20; i++ {
		closes = append(closes, closes[len(closes)-1]*0.98)
	}
	ctx := NewContext("", map[string][]domain.Bar{"AAA": barsFrom("AAA", closes, 1000, nil)}, nil, decimal.Zero)

	if sigs := mustOnBars(t, s, ctx); len(sigs) != 0 {
		t.Errorf("強トレンド時は意見を出さない: %+v", sigs)
	}
}

func TestRSIReversionRejectsBadThresholds(t *testing.T) {
	if _, err := NewRSIReversion(14, 70, 30, 14, 40); err == nil {
		t.Error("oversold > overbought は弾くべき")
	}
}

func mustOnBars(t *testing.T, s Strategy, ctx *Context) []domain.Signal {
	t.Helper()
	sigs, err := s.OnBars(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return sigs
}
