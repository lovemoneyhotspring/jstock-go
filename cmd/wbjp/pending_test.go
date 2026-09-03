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

func TestResolvePendingOrdersNotSentAllowsResend(t *testing.T) {
	rep, req := pendingRepo(t)
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	later := time.Now().UTC().Add(time.Minute)
	summary, err := resolvePendingOrders(rep, &historyBroker{}, logger, later)
	if err != nil || summary.NotSent != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if rep.WasPlaced(req.ClientOrderID) {
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
	if !rep.WasPlaced(req.ClientOrderID) {
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
	if !rep.WasPlaced(req.ClientOrderID) {
		t.Error("判定できないときは PENDING のまま（送り直さない）")
	}
}
