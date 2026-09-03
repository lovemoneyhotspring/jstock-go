package domain

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestMakeClientOrderID(t *testing.T) {
	seedKey := "2026-08-29"
	symbol := "7203"
	side := SideBuy
	qty := decimal.NewFromInt(100)

	id := MakeClientOrderID(seedKey, symbol, side, qty)
	expected := "224e6654a7502b73ce8fb0103ac83a0d"

	if id != expected {
		t.Fatalf("MakeClientOrderID mismatch: got %s, want %s", id, expected)
	}
	if len(id) != 32 {
		t.Fatalf("MakeClientOrderID length must be 32, got %d", len(id))
	}
}

func TestOrderRequestValidation(t *testing.T) {
	qty := decimal.NewFromInt(100)
	price := decimal.NewFromInt(2500)

	// 正当な指値注文
	req, err := NewOrderRequest("id123", "7203", SideBuy, OrderTypeLimit, qty, &price, TaxAccountSpecific, "test", TradeTypeCash)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notional := req.Notional(); notional == nil || !notional.Equal(decimal.NewFromInt(250000)) {
		t.Fatalf("unexpected notional: %v", notional)
	}

	// 成行なのに指値価格が渡されたらエラー
	_, err = NewOrderRequest("id123", "7203", SideBuy, OrderTypeMarket, qty, &price, TaxAccountSpecific, "test", TradeTypeCash)
	if err == nil {
		t.Fatalf("expected error when limit price specified for market order")
	}

	// 指値なのに価格がないとエラー
	_, err = NewOrderRequest("id123", "7203", SideBuy, OrderTypeLimit, qty, nil, TaxAccountSpecific, "test", TradeTypeCash)
	if err == nil {
		t.Fatalf("expected error when limit price is missing for limit order")
	}

	// 負の数量はエラー
	_, err = NewOrderRequest("id123", "7203", SideBuy, OrderTypeLimit, decimal.NewFromInt(-10), &price, TaxAccountSpecific, "test", TradeTypeCash)
	if err == nil {
		t.Fatalf("expected error for negative quantity")
	}
}
