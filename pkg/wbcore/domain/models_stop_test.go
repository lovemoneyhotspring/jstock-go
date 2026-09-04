package domain

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestWithStop(t *testing.T) {
	base, err := NewOrderRequest("id", "7203", SideSell, OrderTypeMarket, decimal.NewFromInt(100), nil,
		TaxAccountSpecific, "stop", TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	// 逆指値だけ（成行）
	req, err := base.WithStop(decimal.NewFromInt(2400), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !req.IsStopOnly() || req.Stop == nil || !req.Stop.Trigger.Equal(decimal.NewFromInt(2400)) || req.Stop.Price != nil {
		t.Errorf("逆指値の指定が違う: %+v", req.Stop)
	}
	// 通常＋逆指値（指値 2600 で出し、2400 以下で 2380 の指値に）
	limit := decimal.NewFromInt(2600)
	after := decimal.NewFromInt(2380)
	combined, err := NewOrderRequest("id2", "7203", SideSell, OrderTypeLimit, decimal.NewFromInt(100), &limit,
		TaxAccountSpecific, "oco", TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	combined, err = combined.WithStop(decimal.NewFromInt(2400), &after)
	if err != nil {
		t.Fatal(err)
	}
	if combined.IsStopOnly() || combined.Stop.Price == nil || !combined.Stop.Price.Equal(after) {
		t.Errorf("通常＋逆指値の指定が違う: %+v", combined.Stop)
	}
	// 条件価格・値段は正
	if _, err := base.WithStop(decimal.Zero, nil); err == nil {
		t.Error("条件価格 0 を通している")
	}
	zero := decimal.Zero
	if _, err := base.WithStop(decimal.NewFromInt(2400), &zero); err == nil {
		t.Error("発火後の値段 0 を通している")
	}
	// 元の注文は変えない
	if base.Stop != nil {
		t.Error("WithStop が元の注文を書き換えている")
	}
}
