package main

import (
	"testing"

	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func dec(v string) *decimal.Decimal {
	d := decimal.RequireFromString(v)
	return &d
}

// 建玉は寄付の成行が分割約定するので、判断時の価格も株数で加重して比べる。
// 単純平均にすると 100 株の 1 本と 500 株の 1 本が同じ重みになり、ずれを読み違える。
func TestDecidedPricesWeightsByQuantity(t *testing.T) {
	orders := []dtledger.Order{
		{Symbol: "5930", Side: domain.SideBuy, Quantity: decimal.NewFromInt(100), Price: dec("1900"), Trade: domain.TradeTypeMarginOpen},
		{Symbol: "5930", Side: domain.SideBuy, Quantity: decimal.NewFromInt(300), Price: dec("1908"), Trade: domain.TradeTypeMarginOpen},
	}
	got := decidedPrices(orders)
	leg := broker.LegOf("5930", domain.TradeTypeMarginOpen, false)
	want := decimal.RequireFromString("1906") // (100*1900 + 300*1908) / 400
	if p, ok := got[leg]; !ok || !p.Equal(want) {
		t.Fatalf("加重平均が違う: got %v (ok=%v), want %v", p, ok, want)
	}
}

// 返済・dry-run・値段の無い注文は建値の比べに使わない。
// 返済を混ぜると手仕舞いの値段が「判断時」に化けて、執行のずれが消える。
func TestDecidedPricesSkipsCloseAndDryRun(t *testing.T) {
	orders := []dtledger.Order{
		{Symbol: "8358", Side: domain.SideBuy, Quantity: decimal.NewFromInt(600), Price: dec("2830"), Trade: domain.TradeTypeMarginOpen},
		{Symbol: "8358", Side: domain.SideSell, Quantity: decimal.NewFromInt(600), Price: dec("2900"), Trade: domain.TradeTypeMarginClose},
		{Symbol: "6284", Side: domain.SideBuy, Quantity: decimal.NewFromInt(200), Price: dec("8470"), Trade: domain.TradeTypeMarginOpen, Status: dtledger.DryRunStatus},
		{Symbol: "2384", Side: domain.SideBuy, Quantity: decimal.NewFromInt(300), Trade: domain.TradeTypeMarginOpen},
	}
	got := decidedPrices(orders)
	if len(got) != 1 {
		t.Fatalf("新規の 1 脚だけが残るはず: %v", got)
	}
	leg := broker.LegOf("8358", domain.TradeTypeMarginOpen, false)
	if p, ok := got[leg]; !ok || !p.Equal(decimal.NewFromInt(2830)) {
		t.Fatalf("新規の値段が違う: got %v (ok=%v)", p, ok)
	}
}

// 売建は同じ銘柄でも別の脚。買建と混ぜると建値が平均されて、どちらのずれも見えなくなる。
func TestDecidedPricesSeparatesShortLeg(t *testing.T) {
	orders := []dtledger.Order{
		{Symbol: "8848", Side: domain.SideBuy, Quantity: decimal.NewFromInt(100), Price: dec("700"), Trade: domain.TradeTypeMarginOpen},
		{Symbol: "8848", Side: domain.SideSell, Quantity: decimal.NewFromInt(100), Price: dec("761"), Trade: domain.TradeTypeMarginOpen},
	}
	got := decidedPrices(orders)
	long := broker.LegOf("8848", domain.TradeTypeMarginOpen, false)
	short := broker.LegOf("8848", domain.TradeTypeMarginOpen, true)
	if p := got[long]; !p.Equal(decimal.NewFromInt(700)) {
		t.Fatalf("買建の値段が違う: %v", p)
	}
	if p := got[short]; !p.Equal(decimal.NewFromInt(761)) {
		t.Fatalf("売建の値段が違う: %v", p)
	}
}
