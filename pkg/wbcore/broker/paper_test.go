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

// 手数料は定額コース: 現物はその日の合計で段階が決まり、信用は 0 円。
func TestPaperBrokerFlatRateCommission(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990), "9984": decimal.NewFromInt(2000)})
	pb.BeginDay()

	place := func(id, sym string, qty int64, trade domain.TradeType) {
		t.Helper()
		req, err := domain.NewOrderRequest(id, sym, domain.SideBuy, domain.OrderTypeMarket,
			decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", trade)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	// 滑りを 0 にして代金を読みやすくする
	pb.slippageRate = decimal.Zero

	// 1 件目: 199,000 円 → 20 万円まで 176 円
	place("b1", "7203", 100, domain.TradeTypeCash)
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(176)) {
		t.Fatalf("1 件目の手数料 = %+v, want 176", fills)
	}
	// 2 件目: 合計 399,000 円 → 50 万円まで 253 円。増えるのは差分の 77 円
	place("b2", "9984", 100, domain.TradeTypeCash)
	fills = pb.Settle(map[string]decimal.Decimal{"9984": decimal.NewFromInt(2000)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(77)) {
		t.Fatalf("2 件目の手数料 = %+v, want 77（合計で段階が上がった差分）", fills)
	}
	// 信用は 0 円
	place("m1", "7203", 100, domain.TradeTypeMarginOpen)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.IsZero() {
		t.Fatalf("信用の手数料 = %+v, want 0", fills)
	}
	// 日が変わると合計は戻る
	pb.BeginDay()
	place("b3", "7203", 100, domain.TradeTypeCash)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(176)) {
		t.Fatalf("翌日の 1 件目の手数料 = %+v, want 176", fills)
	}
}

// 立会いの無かった銘柄の注文は失効させず、次の立会いで約定させる。
func TestPaperBrokerExpiresOnlyTradedSymbols(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000), "SPY": decimal.NewFromInt(500)})
	for _, sym := range []string{"7203", "SPY"} {
		req, _ := domain.NewOrderRequest("o-"+sym, sym, domain.SideBuy, domain.OrderTypeMarket,
			decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	// 米国だけが立ち会った日: SPY の注文は失効、7203 の注文は残る
	pb.ExpireOpenOrdersFor(map[string]struct{}{"SPY": {}})
	open, _ := pb.GetOpenOrders()
	if len(open) != 1 || open[0].Symbol != "7203" {
		t.Fatalf("残る注文 = %+v, want 7203 だけ", open)
	}
	// 次の東証の立会いで約定する
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000)}, nil, nil, nil)
	if len(fills) != 1 || fills[0].Symbol != "7203" {
		t.Fatalf("次の立会いで約定していない: %+v", fills)
	}
	// nil なら全部失効
	req, _ := domain.NewOrderRequest("o-2", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if _, err := pb.Place(req); err != nil {
		t.Fatal(err)
	}
	pb.ExpireOpenOrders()
	if open, _ := pb.GetOpenOrders(); len(open) != 0 {
		t.Errorf("全銘柄の失効で注文が残っている: %+v", open)
	}
}

// 逆指値は条件に触れるまで約定せず、触れたら発火した値段で成行になる。
// 発火前は条件を訂正でき、発火後はできない。
func TestPaperBrokerStopOrder(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "intrabar")
	pb.slippageRate = decimal.Zero
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)})
	buy, _ := domain.NewOrderRequest("b", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "entry", domain.TradeTypeCash)
	if _, err := pb.Place(buy); err != nil {
		t.Fatal(err)
	}
	pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)}, nil, nil, nil)

	sell, _ := domain.NewOrderRequest("s", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "stop", domain.TradeTypeCash)
	stop, err := sell.WithStop(decimal.NewFromInt(2400), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pb.Place(stop); err != nil {
		t.Fatal(err)
	}

	// 安値が条件に届かない日は何もしない
	hi, lo := decimal.NewFromInt(2550), decimal.NewFromInt(2450)
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)},
		map[string]decimal.Decimal{"7203": hi}, map[string]decimal.Decimal{"7203": lo}, nil)
	if len(fills) != 0 {
		t.Fatalf("条件に触れていないのに約定した: %+v", fills)
	}
	// 発火前は条件を引き上げられる
	if err := pb.CorrectStop("s", nil, domain.StopSpec{Trigger: decimal.NewFromInt(2450)}); err != nil {
		t.Fatalf("発火前の訂正が拒否された: %v", err)
	}
	open, _ := pb.GetOpenOrders()
	if len(open) != 1 || open[0].Stop == nil || !open[0].Stop.Trigger.Equal(decimal.NewFromInt(2450)) {
		t.Fatalf("訂正が反映されていない: %+v", open)
	}

	// 安値が条件に触れた日は条件価格で成行約定
	lo = decimal.NewFromInt(2440)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)},
		map[string]decimal.Decimal{"7203": hi}, map[string]decimal.Decimal{"7203": lo}, nil)
	if len(fills) != 1 || !fills[0].Price.Equal(decimal.NewFromInt(2450)) {
		t.Fatalf("逆指値の約定 = %+v, want 2450 で 1 件", fills)
	}
	if err := pb.CorrectStop("s", nil, domain.StopSpec{Trigger: decimal.NewFromInt(2300)}); err == nil {
		t.Error("約定済みの逆指値の訂正を通している")
	}

	// 寄付で条件を割って寄れば寄付の値段（ギャップ分は不利）。別 ID で建て直す
	buy2, _ := domain.NewOrderRequest("b2", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "entry", domain.TradeTypeCash)
	if _, err := pb.Place(buy2); err != nil {
		t.Fatal(err)
	}
	pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)}, nil, nil, nil)
	sell2, _ := domain.NewOrderRequest("s2", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "stop", domain.TradeTypeCash)
	stop2, _ := sell2.WithStop(decimal.NewFromInt(2400), nil)
	if _, err := pb.Place(stop2); err != nil {
		t.Fatal(err)
	}
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2300)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Price.Equal(decimal.NewFromInt(2300)) {
		t.Fatalf("ギャップで抜けた逆指値の約定 = %+v, want 寄付 2300", fills)
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
