package ledger

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// newTestLedger は注文を 1 件だけ記録した台帳を返す。
func newTestLedger(t *testing.T) (*Ledger, domain.OrderRequest) {
	t.Helper()
	l, err := OpenLedger(filepath.Join(t.TempDir(), "accum-uat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })

	price := decimal.NewFromInt(2500)
	req, err := domain.NewOrderRequest("order-1", "1306", domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(10), &price, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	month := "2026-09-01"
	amount := decimal.NewFromInt(25_000)
	market := domain.MarketJP
	if err := l.Record(req, string(domain.OrderStatusSubmitted), nil, &month, &amount, &market); err != nil {
		t.Fatal(err)
	}
	return l, req
}

func TestBackupCopiesTheLedger(t *testing.T) {
	l, _ := newTestLedger(t)
	dest := filepath.Join(t.TempDir(), "nested", "accum-20260903.db")

	path, err := l.Backup(dest)
	if err != nil {
		t.Fatal(err)
	}
	if path != dest {
		t.Errorf("複製先 = %q", path)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("複製が無い: %v", err)
	}
	if info.Size() == 0 {
		t.Error("複製が空")
	}

	// 同じ日に 2 回走っても失敗しない（cron の再試行・手動実行）
	if _, err := l.Backup(dest); err != nil {
		t.Errorf("取り直しに失敗: %v", err)
	}

	// 複製から読み直せる＝一貫したスナップショットになっている
	copied, err := OpenLedger(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if !copied.WasPlaced("order-1") {
		t.Error("複製に注文が入っていない")
	}
}

func TestUpdateStatusDetailOverwritesAmountAndBrokerID(t *testing.T) {
	l, req := newTestLedger(t)

	filled := decimal.NewFromInt(10)
	avg := decimal.NewFromFloat(2400)
	brokerID := "b-999"
	// 約定額（10 株 × 2400 円）で発注済み額を上書きする
	actual := decimal.NewFromInt(24_000)
	if err := l.UpdateStatusDetail(req.ClientOrderID, string(domain.OrderStatusFilled),
		&filled, &avg, &brokerID, &actual); err != nil {
		t.Fatal(err)
	}

	orders, err := l.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("注文が %d 件", len(orders))
	}
	o := orders[0]
	if o.BrokerOrderID == nil || *o.BrokerOrderID != brokerID {
		t.Errorf("broker_order_id = %v", o.BrokerOrderID)
	}
	if o.Amount == nil || !o.Amount.Equal(actual) {
		t.Errorf("amount = %v（約定額で上書きされるはず）", o.Amount)
	}
	if !o.EffectiveAmount().Equal(actual) {
		t.Errorf("有効額 = %s", o.EffectiveAmount())
	}
}

// nil を渡した項目は変えない（照会で分からなかった値で既存を潰さない）。
func TestUpdateStatusKeepsExistingValues(t *testing.T) {
	l, req := newTestLedger(t)
	filled := decimal.NewFromInt(10)
	avg := decimal.NewFromFloat(2400)
	brokerID := "b-1"
	actual := decimal.NewFromInt(24_000)
	if err := l.UpdateStatusDetail(req.ClientOrderID, string(domain.OrderStatusFilled), &filled, &avg, &brokerID, &actual); err != nil {
		t.Fatal(err)
	}
	// 旧来の 4 引数版は broker_order_id / amount を触らない
	if err := l.UpdateStatus(req.ClientOrderID, string(domain.OrderStatusFilled), nil, nil); err != nil {
		t.Fatal(err)
	}
	orders, _ := l.Recent(1)
	o := orders[0]
	if o.BrokerOrderID == nil || *o.BrokerOrderID != brokerID {
		t.Errorf("broker_order_id が消えた: %v", o.BrokerOrderID)
	}
	if o.Amount == nil || !o.Amount.Equal(actual) {
		t.Errorf("amount が消えた: %v", o.Amount)
	}
	if !o.FilledQuantity.Equal(filled) {
		t.Errorf("filled_quantity が消えた: %s", o.FilledQuantity)
	}
}
