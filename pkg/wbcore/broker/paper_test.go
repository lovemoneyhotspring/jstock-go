package broker

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestPaperBroker_Lifecycle(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(1000000), "open")

	// 初期残高チェック
	bal, err := pb.GetBalance()
	if err != nil {
		t.Fatalf("failed to get balance: %v", err)
	}
	if !bal.CashBalance.Equal(decimal.NewFromInt(1000000)) {
		t.Errorf("initial cash = %s, want 1000000", bal.CashBalance)
	}

	// 指値買い発注
	limitPrice := decimal.NewFromInt(2000)
	qty := decimal.NewFromInt(100)
	req, err := domain.NewOrderRequest("order-1", "7203", domain.SideBuy, domain.OrderTypeLimit, qty, &limitPrice, domain.TaxAccountSpecific, "test buy", domain.TradeTypeCash)
	if err != nil {
		t.Fatalf("failed to create order req: %v", err)
	}

	ack, err := pb.Place(req)
	if err != nil {
		t.Fatalf("failed to place order: %v", err)
	}
	if ack.Status != domain.OrderStatusSubmitted {
		t.Errorf("status = %s, want SUBMITTED", ack.Status)
	}

	// 翌日の約定シミュレーション (寄付が 2000 以下なら約定)
	openPrices := map[string]decimal.Decimal{
		"7203": decimal.NewFromInt(1990),
	}
	fills := pb.Settle(openPrices, nil, nil, nil)
	if len(fills) != 1 {
		t.Fatalf("expected 1 fill, got %d", len(fills))
	}
	if !fills[0].Price.Equal(decimal.NewFromInt(1990)) {
		t.Errorf("fill price = %s, want 1990", fills[0].Price)
	}

	// ポジション確認
	positions, err := pb.GetPositions()
	if err != nil {
		t.Fatalf("failed to get positions: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	if !positions[0].Quantity.Equal(qty) {
		t.Errorf("position qty = %s, want %s", positions[0].Quantity, qty)
	}

	// 売り発注
	sellLimit := decimal.NewFromInt(2100)
	sellReq, _ := domain.NewOrderRequest("order-2", "7203", domain.SideSell, domain.OrderTypeLimit, qty, &sellLimit, domain.TaxAccountSpecific, "test sell", domain.TradeTypeCash)
	_, err = pb.Place(sellReq)
	if err != nil {
		t.Fatalf("failed to place sell order: %v", err)
	}

	// 寄付 2110 で約定
	openPrices["7203"] = decimal.NewFromInt(2110)
	sellFills := pb.Settle(openPrices, nil, nil, nil)
	if len(sellFills) != 1 {
		t.Fatalf("expected 1 sell fill, got %d", len(sellFills))
	}

	// 建玉が0になっていること
	positions, _ = pb.GetPositions()
	if len(positions) != 0 {
		t.Errorf("expected 0 positions after sell, got %d", len(positions))
	}

	// 利益が出ていること (売却 2110 - 買付 1990 = 120 * 100 = 12,000円から手数料引いた額)
	if pb.realizedPnL.LessThanOrEqual(decimal.Zero) {
		t.Errorf("expected positive realized pnl, got %s", pb.realizedPnL)
	}
}

// 待機資金の利息は年率を 360 日で日割りする（T-Bill の慣行）。
func TestPaperBrokerAccrueInterest(t *testing.T) {
	p := NewPaperBroker(decimal.NewFromInt(3_600_000), "open")

	// 年 5% を 36 日 → 3,600,000 × 0.05 × 36/360 = 18,000
	got := p.AccrueInterest(decimal.RequireFromString("0.05"), 36)
	if !got.Equal(decimal.NewFromInt(18_000)) {
		t.Errorf("利息 = %s, want 18000", got)
	}
	bal, _ := p.GetBalance()
	if !bal.CashBalance.Equal(decimal.NewFromInt(3_618_000)) {
		t.Errorf("残高 = %s, want 3618000", bal.CashBalance)
	}

	// 金利ゼロ・日数ゼロでは何もしない
	if !p.AccrueInterest(decimal.Zero, 30).IsZero() {
		t.Error("金利ゼロで利息が付きました")
	}
	if !p.AccrueInterest(decimal.RequireFromString("0.05"), 0).IsZero() {
		t.Error("日数ゼロで利息が付きました")
	}
}
