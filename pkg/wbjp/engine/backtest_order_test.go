package engine

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// TestPlaceReviewedSellsFirst は 2026-09-24 の再点検（ライブとの食い違い）の再現。
//
// ライブは売りを先に審査する（risk.SellsFirst）。バックテストが Reconcile の順
// （銘柄コード順: 買い 1001・1002 → 売り 9984）のままだと、max_orders_per_day = 2 を
// 買いで使い切り、損切りの売りを見送っていた。
func TestPlaceReviewedSellsFirst(t *testing.T) {
	d := decimal.RequireFromString
	req := func(id, sym string, side domain.Side) *domain.OrderRequest {
		r, err := domain.NewOrderRequest(id, sym, side, domain.OrderTypeMarket,
			d("100"), nil, domain.TaxAccountSpecific, "", domain.TradeTypeCash)
		if err != nil {
			t.Fatal(err)
		}
		return &r
	}
	orders := []ReconcileResult{
		{Symbol: "1001", Side: domain.SideBuy, Request: req("a", "1001", domain.SideBuy)},
		{Symbol: "1002", Side: domain.SideBuy, Request: req("b", "1002", domain.SideBuy)},
		{Symbol: "5000", Reason: "見送り"}, // Request が無い行は飛ばす
		{Symbol: "9984", Side: domain.SideSell, Request: req("s", "9984", domain.SideSell)},
	}
	mgr := risk.NewRiskManager(wbjpcfg.RiskConfig{
		MaxOrderValue: d("10000000"), MaxOrdersPerDay: 2, MaxDailyLoss: d("1000000"),
		MaxPositionWeight: d("1"), MaxGrossExposure: d("10"),
	}, []string{"1001", "1002", "9984"})
	ctx := risk.RiskContext{
		Equity:  d("10000000"),
		Balance: domain.Balance{BuyingPower: d("10000000")},
		Positions: map[string]domain.Position{
			"9984": {Symbol: "9984", Quantity: d("100"), AvailableQuantity: d("100"), CostPrice: d("1200")},
		},
		BasePrices:   map[string]decimal.Decimal{"1001": d("1000"), "1002": d("1000"), "9984": d("1000")},
		PendingValue: map[string]decimal.Decimal{},
	}
	var placed []string
	placeReviewed(orders, mgr, &ctx, func(r domain.OrderRequest) { placed = append(placed, r.ClientOrderID) })
	if len(placed) != 2 || placed[0] != "s" || placed[1] != "a" {
		t.Fatalf("通った注文: %v（損切りの売り s と最初の買い a のはず）", placed)
	}
	if ctx.OrdersToday != 2 {
		t.Errorf("当日の件数が進まない: %d", ctx.OrdersToday)
	}
}
