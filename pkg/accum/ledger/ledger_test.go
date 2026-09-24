package ledger

import (
	"os"
	"path/filepath"
	"strings"
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

	if placed, err := l.WasPlaced(req.ClientOrderID); err != nil || placed {
		t.Fatalf("expected WasPlaced to be false before record (err: %v)", err)
	}

	month := "2026-08-01"
	amt := decimal.NewFromInt(250000)
	mkt := domain.MarketJP
	brokerID := "broker-123"

	// 注文を記録
	if err := l.Record(req, string(domain.OrderStatusSubmitted), &brokerID, &month, &amt, &mkt); err != nil {
		t.Fatalf("failed to record order: %v", err)
	}

	if placed, err := l.WasPlaced(req.ClientOrderID); err != nil || !placed {
		t.Fatalf("expected WasPlaced to be true after record (err: %v)", err)
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

// 拒否・未送信・dry-run は「出していない」。それ以外（送信中・受理・一部約定・約定・取消・失効・
// 不明）は一度ブローカーに届いた（かもしれない）注文なので、同じ ID を再送させない。
// 状態だけで決まり、約定 0 株の取消・失効も「出した」（daytrade とは違う。理由は WasPlaced のコメント）。
func TestWasPlacedByStatus(t *testing.T) {
	l, err := OpenLedger(filepath.Join(t.TempDir(), "accum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	cases := []struct {
		status string
		want   bool
	}{
		{string(domain.OrderStatusPending), true},
		{string(domain.OrderStatusSubmitted), true},
		{string(domain.OrderStatusPartiallyFilled), true},
		{string(domain.OrderStatusFilled), true},
		{string(domain.OrderStatusUnknown), true},
		{string(domain.OrderStatusCancelled), true},
		{string(domain.OrderStatusExpired), true},
		{string(domain.OrderStatusRejected), false},
		{string(domain.OrderStatusUnsent), false},
		{DryRunStatus, false},
	}
	price := decimal.NewFromInt(1000)
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			id := "order-" + tc.status
			req, err := domain.NewOrderRequest(id, "1306", domain.SideBuy, domain.OrderTypeLimit,
				decimal.NewFromInt(100), &price, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.Record(req, tc.status, nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			placed, err := l.WasPlaced(id)
			if err != nil {
				t.Fatalf("台帳を読めません: %v", err)
			}
			if placed != tc.want {
				t.Errorf("WasPlaced(%s) = %v, want %v", tc.status, placed, tc.want)
			}
		})
	}

	// 一部約定してから取り消された注文も「出した」（状態だけで決まる）
	partial := decimal.NewFromInt(50)
	if err := l.UpdateStatus("order-"+string(domain.OrderStatusCancelled), string(domain.OrderStatusCancelled), &partial, &price); err != nil {
		t.Fatal(err)
	}
	if placed, err := l.WasPlaced("order-" + string(domain.OrderStatusCancelled)); err != nil || !placed {
		t.Errorf("一部約定の取消: WasPlaced = (%v, %v), want (true, nil)", placed, err)
	}

	// 台帳に無い ID はエラーではなく「出していない」
	if placed, err := l.WasPlaced("無い注文"); err != nil || placed {
		t.Errorf("WasPlaced(無い注文) = (%v, %v), want (false, nil)", placed, err)
	}
}

// 台帳が読めないときに「出していない」と答えると、プロセスをまたいだ二重発注の柵が外れる。
func TestWasPlacedFailsClosedWhenLedgerUnreadable(t *testing.T) {
	l, err := OpenLedger(filepath.Join(t.TempDir(), "accum.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	placed, err := l.WasPlaced("order-1")
	if err == nil {
		t.Fatal("閉じた台帳でもエラーにならない（読めないのを「出していない」と読んでいる）")
	}
	if placed {
		t.Error("エラーのときに発注済みと答えている")
	}
	if !strings.Contains(err.Error(), "order-1") {
		t.Errorf("どの注文か分からない: %v", err)
	}
}
