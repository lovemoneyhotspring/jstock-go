package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/shopspring/decimal"
)

type historyBroker struct {
	broker.Broker
	history []domain.Order
	err     error
}

func (h *historyBroker) GetOrderHistory(time.Time, time.Time) ([]domain.Order, error) {
	return h.history, h.err
}

func pendingRepo(t *testing.T) (*repo.Repo, domain.OrderRequest) {
	t.Helper()
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	if err := rep.StartRun("run-1", "2026-09-04", "uat", "live"); err != nil {
		t.Fatal(err)
	}
	limit := decimal.NewFromInt(1000)
	req, err := domain.NewOrderRequest("cid-1", "7203", domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(100), &limit, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	if err := rep.RecordOrder("run-1", req, string(domain.OrderStatusPending), nil); err != nil {
		t.Fatal(err)
	}
	return rep, req
}

func wasPlaced(t *testing.T, rep *repo.Repo, clientOrderID string) bool {
	t.Helper()
	placed, err := rep.WasPlaced(clientOrderID)
	if err != nil {
		t.Fatal(err)
	}
	return placed
}

func TestResolvePendingOrdersNotSentAllowsResend(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	later := time.Now().UTC().Add(time.Minute)
	summary, err := resolvePendingOrders(rep, &historyBroker{}, logger, later)
	if err != nil || summary.NotSent != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if wasPlaced(t, rep, req.ClientOrderID) {
		t.Error("UNSENT は発注済みに数えない（送り直せる）")
	}
}

func TestResolvePendingOrdersAttributes(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	id := "55/20260904"
	created := time.Now().UTC()
	b := &historyBroker{history: []domain.Order{{
		ClientOrderID: id, BrokerOrderID: &id, Symbol: "7203", Side: domain.SideBuy,
		Quantity: req.Quantity, FilledQuantity: decimal.Zero, Status: domain.OrderStatusSubmitted, CreatedAt: &created,
	}}}
	summary, err := resolvePendingOrders(rep, b, logger, time.Now().UTC().Add(time.Minute))
	if err != nil || summary.Attributed != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	open, _ := rep.UnresolvedOrders()
	if len(open) != 1 || open[0].BrokerOrderID == nil || *open[0].BrokerOrderID != id || open[0].Status != domain.OrderStatusSubmitted {
		t.Errorf("帰属が台帳に無い: %+v", open)
	}
	if !wasPlaced(t, rep, req.ClientOrderID) {
		t.Error("板に残っている注文は発注済み")
	}
}

func TestResolvePendingOrdersFailsClosedWhenHistoryUnavailable(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	_, err := resolvePendingOrders(rep, &historyBroker{err: errors.New("down")}, logger, time.Now().UTC().Add(time.Minute))
	if err == nil {
		t.Fatal("一覧を照会できないのに通った")
	}
	if !wasPlaced(t, rep, req.ClientOrderID) {
		t.Error("判定できないときは PENDING のまま（送り直さない）")
	}
}

// TestResolvePendingOrdersEmptyListIsNotTrusted は W3 の再現。今日送って注文番号まで分かっている
// 注文があるのに一覧が空で返ったら、PENDING を UNSENT にしない（送り直すと二重発注）。
func TestResolvePendingOrdersEmptyListIsNotTrusted(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	// 同じ日に送って受理された別の注文（注文番号あり）
	limit := decimal.NewFromInt(2000)
	other, err := domain.NewOrderRequest("cid-2", "6758", domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(100), &limit, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	id := "77/20260904"
	if err := rep.RecordOrder("run-1", other, string(domain.OrderStatusSubmitted), &id); err != nil {
		t.Fatal(err)
	}
	summary, err := resolvePendingOrders(rep, &historyBroker{}, logger, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if summary.NotSent != 0 || summary.TooRecent != 1 {
		t.Errorf("空の一覧を信用して届いていないと決めた: %+v", summary)
	}
	if !wasPlaced(t, rep, req.ClientOrderID) {
		t.Error("PENDING のまま残すべき（送り直さない）")
	}
}

// TestResolvePendingOrdersSkipsEarlierDays は、今日より前に送った PENDING を当日分しか返らない
// 一覧で「届いていない」と決めない（UNSENT にすると翌日以降に送り直してしまう）。
func TestResolvePendingOrdersSkipsEarlierDays(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	summary, err := resolvePendingOrders(rep, &historyBroker{}, logger, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if summary.NotSent != 0 {
		t.Errorf("前日の PENDING を届いていないと決めた: %+v", summary)
	}
	if !wasPlaced(t, rep, req.ClientOrderID) {
		t.Error("前日の PENDING は PENDING のまま残すべき")
	}
}
