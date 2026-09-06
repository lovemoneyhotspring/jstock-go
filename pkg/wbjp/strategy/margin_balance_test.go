package strategy

import (
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func marginBalanceStrategy(t *testing.T, mutate func(*MarginBalanceOptions)) *MarginBalance {
	t.Helper()
	o := DefaultMarginBalanceOptions()
	if mutate != nil {
		mutate(&o)
	}
	s, err := NewMarginBalance(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarginBalanceFilterVetoesOnlyTheBottom(t *testing.T) {
	bars, book := marginUniverse()
	ctx := NewUniverse(bars).SetMargin(book).At("", nil, decimal.Zero)
	sigs := mustOnBars(t, marginBalanceStrategy(t, nil), ctx)

	if len(sigs) != 1 {
		t.Fatalf("下位 20%%（AAA だけ）に意見が出るはず: %v", symbolsOf(sigs))
	}
	sig := mustSignal(t, sigs, "AAA")
	if sig.Direction != -1 || !strings.Contains(sig.Reason, "売り長") {
		t.Errorf("売り長は −1: %+v", sig)
	}
	if sig.Meta["margin_rank"].(float64) != 0.1 {
		t.Errorf("meta に分位が要る: %v", sig.Meta)
	}
}

func TestMarginBalanceFilterKeepsHeldUnlessAsked(t *testing.T) {
	bars, book := marginUniverse()
	held := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 100)}
	ctx := NewUniverse(bars).SetMargin(book).At("", held, decimal.Zero)

	if sigs := mustOnBars(t, marginBalanceStrategy(t, nil), ctx); len(sigs) != 0 {
		t.Errorf("保有中の銘柄には既定では黙るはず: %v", symbolsOf(sigs))
	}
	s := marginBalanceStrategy(t, func(o *MarginBalanceOptions) { o.ExitHeld = true })
	if sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA"); sig.Direction != -1 {
		t.Errorf("exit_held なら保有中でも −1: %+v", sig)
	}
}

func TestMarginBalanceTiltFavorsTheTop(t *testing.T) {
	bars, book := marginUniverse()
	ctx := NewUniverse(bars).SetMargin(book).At("", nil, decimal.Zero)
	s := marginBalanceStrategy(t, func(o *MarginBalanceOptions) { o.Mode = "tilt" })
	sigs := mustOnBars(t, s, ctx)

	top := mustSignal(t, sigs, "EEE")
	if top.Direction < directionFloor || !strings.Contains(top.Reason, "買い長") {
		t.Errorf("上位 20%% には買い意見: %+v", top)
	}
	if _, ok := signalFor(t, sigs, "CCC"); ok {
		t.Error("中位には意見を出さない")
	}
	if len(sigs) != 2 {
		t.Errorf("AAA（−1）と EEE（買い）だけのはず: %v", symbolsOf(sigs))
	}
}

func TestMarginBalanceSilentWithoutData(t *testing.T) {
	bars, _ := marginUniverse()
	ctx := NewContext("", bars, nil, decimal.Zero)
	if sigs := mustOnBars(t, marginBalanceStrategy(t, nil), ctx); len(sigs) != 0 {
		t.Errorf("信用残が無ければ黙る: %v", symbolsOf(sigs))
	}
}

func TestMarginBalanceRejectsBadOptions(t *testing.T) {
	for _, mutate := range []func(*MarginBalanceOptions){
		func(o *MarginBalanceOptions) { o.Mode = "veto" },
		func(o *MarginBalanceOptions) { o.AvoidBelow = 0.9 },
		func(o *MarginBalanceOptions) { o.FavorAbove = 1.5 },
	} {
		o := DefaultMarginBalanceOptions()
		mutate(&o)
		if _, err := NewMarginBalance(o); err == nil {
			t.Errorf("弾くはず: %+v", o)
		}
	}
}
