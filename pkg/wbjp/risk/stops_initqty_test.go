package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// TestEnsureRaisesInitialQuantityBeforeScaleOut は 2026-09-24 の再点検の再現。
//
// 買いが分割で約定し、ストップを作った回（100 株）の後に 400 株へ増えた。以前は利確の
// 残り株数を作った回の 100 株から決め（半分利確で残り 0 株＝全株売り）ていた。
// 利確前なら基準を 400 株へ引き上げ、半分（残り 200 株）だけ売る。利確後は引き上げない。
func TestEnsureRaisesInitialQuantityBeforeScaleOut(t *testing.T) {
	d := decimal.RequireFromString
	initStop, initQty := d("900"), d("100")
	sb := NewStopBook(map[string]*Stop{"7203": {
		Symbol: "7203", EntryPrice: d("1000"), StopPrice: d("900"), CreatedOn: "2026-09-01",
		ATRMultiple: d("2"), InitialStopPrice: &initStop, InitialQuantity: &initQty,
	}})
	positions := map[string]domain.Position{"7203": {Symbol: "7203", Quantity: d("400"), CostPrice: d("1000")}}
	sb.Ensure(positions, nil, "2026-09-24", d("2"), false)

	st, _ := sb.Get("7203")
	if st.InitialQuantity == nil || !st.InitialQuantity.Equal(d("400")) {
		t.Fatalf("利確前の基準株数が引き上がらない: %v", st.InitialQuantity)
	}
	targetR := d("2")
	targets := sb.TakeProfitTargets(
		map[string]decimal.Decimal{"7203": d("1250")},
		map[string]decimal.Decimal{"7203": d("400")},
		map[string]decimal.Decimal{"7203": d("100")},
		&targetR, d("0.5"), d("100"), false)
	if len(targets) != 1 || !targets[0].Quantity.Equal(d("200")) {
		t.Fatalf("利確の目標（残り 200 株）: %+v", targets)
	}

	// 減った分では引き下げない（利確の売りの途中で基準が動かない）
	sb.Ensure(map[string]domain.Position{"7203": {Symbol: "7203", Quantity: d("200"), CostPrice: d("1000")}},
		nil, "2026-09-25", d("2"), false)
	if st, _ := sb.Get("7203"); !st.InitialQuantity.Equal(d("400")) {
		t.Errorf("減った分で基準を下げた: %v", st.InitialQuantity)
	}

	// 利確後は増えても引き上げない
	scaled, scaledQty := d("900"), d("100")
	sb2 := NewStopBook(map[string]*Stop{"7203": {
		Symbol: "7203", EntryPrice: d("1000"), StopPrice: d("1000"), CreatedOn: "2026-09-01",
		InitialStopPrice: &scaled, InitialQuantity: &scaledQty, ScaledOut: true,
	}})
	sb2.Ensure(positions, nil, "2026-09-24", d("2"), false)
	if st, _ := sb2.Get("7203"); !st.InitialQuantity.Equal(d("100")) {
		t.Errorf("利確後に基準を引き上げた: %v", st.InitialQuantity)
	}
}
