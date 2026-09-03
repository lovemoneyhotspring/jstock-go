package execute

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// lookupBroker は client_order_id で引ける注文だけを返す。表に無い注文は
// 「ブローカーが知らない」＝ nil を返す（応答が返らなかった注文の再現）。
type lookupBroker struct {
	broker.Broker
	orders map[string]*domain.Order
}

func (l *lookupBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	return l.orders[clientOrderID], nil
}

func openTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	led, err := ledger.OpenLedger(filepath.Join(t.TempDir(), "accum-test.db"))
	if err != nil {
		t.Fatalf("台帳を開けません: %v", err)
	}
	t.Cleanup(func() { led.Close() })
	return led
}

// recordPending は PENDING の注文を 1 件、台帳に入れる。
func recordPending(t *testing.T, led *ledger.Ledger, id, symbol string, qty, amount int64) domain.OrderRequest {
	t.Helper()
	price := decimal.NewFromInt(amount / qty)
	req, err := domain.NewOrderRequest(id, symbol, domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(qty), &price, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatalf("注文を作れません: %v", err)
	}
	month := "2026-08-01"
	amt := decimal.NewFromInt(amount)
	market := domain.MarketJP
	if err := led.Record(req, string(domain.OrderStatusPending), nil, &month, &amt, &market); err != nil {
		t.Fatalf("台帳に記録できません: %v", err)
	}
	return req
}

// 応答が返らず PENDING のまま残った注文は、猶予を過ぎたら REJECTED に落とす。
// これをしないと、届かなかった注文が永久に「発注済み」として当月の予算を食う。
func TestSyncOrderStatusRejectsStalePending(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	b := &lookupBroker{orders: map[string]*domain.Order{}} // ブローカーは知らない
	later := time.Now().UTC().Add(UnconfirmedGrace + time.Hour)

	changes, err := SyncOrderStatus(led, b, later)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("変化の件数 = %d, want 1", len(changes))
	}
	if changes[0].After != domain.OrderStatusRejected {
		t.Errorf("状態 = %s, want REJECTED", changes[0].After)
	}
	if !changes[0].LostAmountRatio().Equal(decimal.NewFromInt(1)) {
		t.Errorf("未約定割合 = %s, want 1", changes[0].LostAmountRatio())
	}

	// 「発注済み」から外れ、次の run で差額として埋め直される
	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, err := led.PlacedAmount("1306.T", month)
	if err != nil {
		t.Fatalf("発注済み額を引けません: %v", err)
	}
	if !placed.IsZero() {
		t.Errorf("発注済み額 = %s, want 0", placed)
	}
}

// 猶予の内側では触らない。送った直後は照会に反映されていないことがあり、
// ここで失効にすると板に残った注文と二重になる。
func TestSyncOrderStatusKeepsFreshPending(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	b := &lookupBroker{orders: map[string]*domain.Order{}}
	soon := time.Now().UTC().Add(time.Hour)

	changes, err := SyncOrderStatus(led, b, soon)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("変化の件数 = %d, want 0", len(changes))
	}
	open, _ := led.OpenOrders()
	if len(open) != 1 || open[0].Status != string(domain.OrderStatusPending) {
		t.Errorf("PENDING のまま残っていません: %+v", open)
	}
}

// 約定した注文は「発注済み」の額を 株数 × 約定単価 に置き換える。
// 判断時の想定額のままだと、当月の残りの計算が実際に払った額とずれる。
func TestSyncOrderStatusOverwritesAmountWithFillPrice(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000) // 想定 @2500

	avg := decimal.NewFromInt(2400) // 実際は @2400 で約定
	b := &lookupBroker{orders: map[string]*domain.Order{
		req.ClientOrderID: {
			ClientOrderID:  req.ClientOrderID,
			Symbol:         "1306.T",
			Side:           domain.SideBuy,
			Quantity:       decimal.NewFromInt(100),
			FilledQuantity: decimal.NewFromInt(100),
			Status:         domain.OrderStatusFilled,
			AvgFillPrice:   &avg,
		},
	}}

	changes, err := SyncOrderStatus(led, b, time.Now().UTC())
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(changes) != 1 || changes[0].After != domain.OrderStatusFilled {
		t.Fatalf("変化 = %+v", changes)
	}

	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(240_000)) {
		t.Errorf("発注済み額 = %s, want 240000", placed)
	}
}

// 一部だけ約定して失効した注文は、約定したぶんだけを「発注済み」に数える。
// amount に 株数 × 単価 を入れておき、按分は EffectiveAmount に一度だけさせる。
func TestSyncOrderStatusProratesPartialFill(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	avg := decimal.NewFromInt(2500)
	b := &lookupBroker{orders: map[string]*domain.Order{
		req.ClientOrderID: {
			ClientOrderID:  req.ClientOrderID,
			Symbol:         "1306.T",
			Side:           domain.SideBuy,
			Quantity:       decimal.NewFromInt(100),
			FilledQuantity: decimal.NewFromInt(40),
			Status:         domain.OrderStatusExpired,
			AvgFillPrice:   &avg,
		},
	}}

	if _, err := SyncOrderStatus(led, b, time.Now().UTC()); err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}

	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(100_000)) { // 40 株 × 2500
		t.Errorf("発注済み額 = %s, want 100000", placed)
	}
}
