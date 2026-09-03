package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestLedger_Lifecycle(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "accum_ledger_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	dbPath := filepath.Join(tempDir, "accum-uat.db")
	l, err := OpenLedger(dbPath)
	if err != nil {
		t.Fatalf("failed to open ledger: %v", err)
	}
	defer l.Close()

	symbol := "1306"
	qty := decimal.NewFromInt(100)
	price := decimal.NewFromInt(2500)
	req, _ := domain.NewOrderRequest("order-accum-1", symbol, domain.SideBuy, domain.OrderTypeLimit, qty, &price, domain.TaxAccountSpecific, "accum test", domain.TradeTypeCash)

	if l.WasPlaced(req.ClientOrderID) {
		t.Fatalf("expected WasPlaced to be false before record")
	}

	month := "2026-08-01"
	amt := decimal.NewFromInt(250000)
	mkt := domain.MarketJP
	brokerID := "broker-123"

	// 注文を記録
	if err := l.Record(req, string(domain.OrderStatusSubmitted), &brokerID, &month, &amt, &mkt); err != nil {
		t.Fatalf("failed to record order: %v", err)
	}

	if !l.WasPlaced(req.ClientOrderID) {
		t.Fatalf("expected WasPlaced to be true after record")
	}

	// 当月の発注済み額
	mDate, _ := time.Parse("2006-01-02", month)
	placed, err := l.PlacedAmount(symbol, mDate)
	if err != nil {
		t.Fatalf("failed to get placed amount: %v", err)
	}
	if !placed.Equal(amt) {
		t.Errorf("placed amount = %s, want %s", placed, amt)
	}

	// オープン注文の照会
	openOrders, err := l.OpenOrders()
	if err != nil || len(openOrders) != 1 {
		t.Fatalf("expected 1 open order, got %d (err: %v)", len(openOrders), err)
	}
	if openOrders[0].ClientOrderID != req.ClientOrderID {
		t.Errorf("expected client_order_id %s, got %s", req.ClientOrderID, openOrders[0].ClientOrderID)
	}

	// 約定ステータス更新
	filledQty := qty
	avgPrice := price
	if err := l.UpdateStatus(req.ClientOrderID, string(domain.OrderStatusFilled), &filledQty, &avgPrice); err != nil {
		t.Fatalf("failed to update status: %v", err)
	}

	openOrders, _ = l.OpenOrders()
	if len(openOrders) != 0 {
		t.Errorf("expected 0 open orders after fill, got %d", len(openOrders))
	}
}
