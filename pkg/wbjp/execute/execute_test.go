package execute

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// stubBroker は発注の規則を検証するための模型。応答を差し替えられる。
type stubBroker struct {
	place     func(domain.OrderRequest) (*domain.OrderAck, error)
	getOrder  func(clientOrderID string, brokerOrderID *string) (*domain.Order, error)
	cancelErr error
	placed    []domain.OrderRequest
	queried   []string
	cancelled []string
}

func (s *stubBroker) Name() string                         { return "stub" }
func (s *stubBroker) AccountID() string                    { return "stub" }
func (s *stubBroker) GetBalance() (*domain.Balance, error) { return &domain.Balance{}, nil }
func (s *stubBroker) GetPositions() ([]domain.Position, error) {
	return nil, nil
}
func (s *stubBroker) GetOpenOrders() ([]domain.Order, error) { return nil, nil }
func (s *stubBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	s.queried = append(s.queried, clientOrderID)
	if s.getOrder == nil {
		return nil, errors.New("照会できない")
	}
	return s.getOrder(clientOrderID, brokerOrderID)
}
func (s *stubBroker) GetOrderHistory(_, _ time.Time) ([]domain.Order, error) { return nil, nil }
func (s *stubBroker) Preview(domain.OrderRequest) (*domain.OrderPreview, error) {
	return nil, nil
}
func (s *stubBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.placed = append(s.placed, req)
	if s.place != nil {
		return s.place(req)
	}
	id := "N/" + req.ClientOrderID
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
}
func (s *stubBroker) Cancel(clientOrderID string, _ *string) error {
	s.cancelled = append(s.cancelled, clientOrderID)
	return s.cancelErr
}
func (s *stubBroker) LotSizes([]string) map[string]decimal.Decimal { return nil }
func (s *stubBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	return map[string]domain.Position{}, nil
}

var _ broker.Broker = (*stubBroker)(nil)

type recorder struct{ errors []string }

func (r *recorder) Info(string, string, ...map[string]any) {}
func (r *recorder) Warn(string, string, ...map[string]any) {}
func (r *recorder) Error(code, msg string, _ ...map[string]any) {
	r.errors = append(r.errors, code+": "+msg)
}

func openRepo(t *testing.T) *repo.Repo {
	t.Helper()
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	if err := rep.StartRun("run-1", "2026-09-14", "prod", "live"); err != nil {
		t.Fatal(err)
	}
	return rep
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// buy は指値 price 円 × qty 株の買い。
func buy(t *testing.T, id, symbol string, price, qty int64) domain.OrderRequest {
	t.Helper()
	limit := decimal.NewFromInt(price)
	req, err := domain.NewOrderRequest(id, symbol, domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(qty), &limit, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func orderOf(t *testing.T, rep *repo.Repo, id string) *repo.OrderRecord {
	t.Helper()
	o, err := rep.GetOrder(id)
	if err != nil || o == nil {
		t.Fatalf("台帳に %s が無い: %v", id, err)
	}
	return o
}

func wasPlaced(t *testing.T, rep *repo.Repo, id string) bool {
	t.Helper()
	placed, err := rep.WasPlaced(id)
	if err != nil {
		t.Fatal(err)
	}
	return placed
}

// --- PlaceRecorded ----------------------------------------------------------

func TestPlaceRecordedAcceptedKeepsBrokerOrderID(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{}
	req := buy(t, "cid-1", "7203", 1000, 100)
	if err := PlaceRecorded(rep, b, "run-1", req, &recorder{}); err != nil {
		t.Fatal(err)
	}
	o := orderOf(t, rep, "cid-1")
	if o.Status != domain.OrderStatusSubmitted || o.BrokerOrderID == nil || *o.BrokerOrderID != "N/cid-1" {
		t.Errorf("受理が台帳に無い: %+v", o)
	}
	if !wasPlaced(t, rep, "cid-1") {
		t.Error("受理された注文は発注済み")
	}
}

func TestPlaceRecordedSentNothingIsResendable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domain.OrderStatus
	}{
		{"拒否", &broker.OrderRejectedError{Message: "余力不足"}, domain.OrderStatusRejected},
		{"締め切り", &broker.ErrDeadline{CLMID: "CLMKabuNewOrder", Deadline: time.Now()}, domain.OrderStatusUnsent},
		{"送る前の失敗", &broker.ErrNotSent{ClientOrderID: "cid-1", Err: errors.New("建玉照会")}, domain.OrderStatusUnsent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := openRepo(t)
			b := &stubBroker{place: func(domain.OrderRequest) (*domain.OrderAck, error) { return nil, c.err }}
			err := PlaceRecorded(rep, b, "run-1", buy(t, "cid-1", "7203", 1000, 100), &recorder{})
			var unconfirmed *ErrUnconfirmedOrder
			if err == nil || errors.As(err, &unconfirmed) {
				t.Fatalf("元のエラーが返っていない: %v", err)
			}
			if o := orderOf(t, rep, "cid-1"); o.Status != c.want {
				t.Errorf("状態 %s（期待 %s）", o.Status, c.want)
			}
			if wasPlaced(t, rep, "cid-1") {
				t.Error("送っていない注文は次回送り直せる")
			}
		})
	}
}

func TestPlaceRecordedUnconfirmedStaysPending(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{place: func(domain.OrderRequest) (*domain.OrderAck, error) { return nil, errors.New("timeout") }}
	err := PlaceRecorded(rep, b, "run-1", buy(t, "cid-1", "7203", 1000, 100), &recorder{})
	var unconfirmed *ErrUnconfirmedOrder
	if !errors.As(err, &unconfirmed) || unconfirmed.ClientOrderID != "cid-1" {
		t.Fatalf("結果不明になっていない: %v", err)
	}
	if o := orderOf(t, rep, "cid-1"); o.Status != domain.OrderStatusPending {
		t.Errorf("結果不明は PENDING のまま: %s", o.Status)
	}
	if !wasPlaced(t, rep, "cid-1") {
		t.Error("届いたかもしれない注文は送り直さない")
	}
}

func TestPlaceRecordedStopsWhenLedgerUnwritable(t *testing.T) {
	rep := openRepo(t)
	_ = rep.Close()
	b := &stubBroker{}
	if err := PlaceRecorded(rep, b, "run-1", buy(t, "cid-1", "7203", 1000, 100), &recorder{}); err == nil {
		t.Fatal("記録できないのに通った")
	}
	if len(b.placed) != 0 {
		t.Error("記録できないまま送った")
	}
}

// --- PlaceOrders ------------------------------------------------------------

func riskManager(symbols ...string) *risk.RiskManager {
	return risk.NewRiskManager(wbjpcfg.RiskConfig{
		MaxOrderValue:     dec("10000000"),
		MaxOrdersPerDay:   10,
		MaxDailyLoss:      dec("1000000"),
		MaxPositionWeight: dec("1"),
		MaxGrossExposure:  dec("10"),
	}, symbols)
}

func riskContext(buyingPower string, prices map[string]decimal.Decimal) *risk.RiskContext {
	return &risk.RiskContext{
		Equity:       dec("10000000"),
		Balance:      domain.Balance{BuyingPower: dec(buyingPower)},
		Positions:    map[string]domain.Position{},
		BasePrices:   prices,
		PendingValue: map[string]decimal.Decimal{},
	}
}

// TestPlaceOrdersReservesBuyingPower は、受理した買いのぶん余力を減らし、
// 余力 100 万円に 40 万円の買いを 3 本通さないこと。
func TestPlaceOrdersReservesBuyingPower(t *testing.T) {
	for _, live := range []bool{true, false} {
		rep := openRepo(t)
		b := &stubBroker{}
		prices := map[string]decimal.Decimal{"7203": dec("4000"), "6758": dec("4000"), "9984": dec("4000")}
		ctx := riskContext("1000000", prices)
		orders := []domain.OrderRequest{
			buy(t, "a", "7203", 4000, 100), buy(t, "b", "6758", 4000, 100), buy(t, "c", "9984", 4000, 100),
		}
		res, err := PlaceOrders(rep, b, orders, ctx, Options{RunID: "run-1", Live: live,
			Risk: riskManager("7203", "6758", "9984"), Report: &recorder{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Placed != 2 || !strings.Contains(res.RiskRejected["9984"], "買付余力") {
			t.Errorf("live=%v: 余力を超えて通した: %+v", live, res)
		}
		if !ctx.Balance.BuyingPower.Equal(dec("200000")) || ctx.OrdersToday != 2 {
			t.Errorf("live=%v: 余力 %s 件数 %d", live, ctx.Balance.BuyingPower, ctx.OrdersToday)
		}
		if !ctx.PendingValue["7203"].Equal(dec("400000")) || !ctx.PendingValue["6758"].Equal(dec("400000")) {
			t.Errorf("live=%v: 未約定に足していない: %v", live, ctx.PendingValue)
		}
		if live && len(b.placed) != 2 {
			t.Errorf("送った数 %d", len(b.placed))
		}
		if !live {
			if len(b.placed) != 0 {
				t.Error("dry-run が送った")
			}
			if o := orderOf(t, rep, "a"); string(o.Status) != DryRunStatus {
				t.Errorf("dry-run の記録: %s", o.Status)
			}
		}
	}
}

// TestPlaceOrdersPendingCountsTowardWeight は、同じ銘柄に通した買いが比率上限の計算に入ること。
func TestPlaceOrdersPendingCountsTowardWeight(t *testing.T) {
	rep := openRepo(t)
	mgr := risk.NewRiskManager(wbjpcfg.RiskConfig{
		MaxOrderValue: dec("10000000"), MaxOrdersPerDay: 10, MaxDailyLoss: dec("1000000"),
		MaxPositionWeight: dec("0.5"), MaxGrossExposure: dec("10"),
	}, []string{"7203"})
	ctx := riskContext("10000000", map[string]decimal.Decimal{"7203": dec("4000")})
	ctx.Equity = dec("1000000")
	orders := []domain.OrderRequest{buy(t, "a", "7203", 4000, 100), buy(t, "b", "7203", 4000, 100)}
	res, err := PlaceOrders(rep, &stubBroker{}, orders, ctx, Options{RunID: "run-1", Live: true, Risk: mgr, Report: &recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Placed != 1 || res.RiskRejected["7203"] == "" {
		t.Errorf("比率上限を超えて通した: %+v", res)
	}
}

// TestSellsFirstKeepsStopLossUnderDailyCap は 2026-09-24 の再点検の再現。
//
// max_orders_per_day は売りにも効く。銘柄コード順（買い 1001・1002 → 売り 9984）のまま
// 審査すると、上限 2 件を買いで使い切り、損切りの売りが見送られた。売りを先に並べる。
func TestSellsFirstKeepsStopLossUnderDailyCap(t *testing.T) {
	limit := dec("1000")
	sell, err := domain.NewOrderRequest("s", "9984", domain.SideSell, domain.OrderTypeLimit,
		dec("100"), &limit, domain.TaxAccountSpecific, "損切り", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	orders := []domain.OrderRequest{buy(t, "a", "1001", 1000, 100), buy(t, "b", "1002", 1000, 100), sell}

	sorted := SellsFirst(orders)
	if got := []string{sorted[0].ClientOrderID, sorted[1].ClientOrderID, sorted[2].ClientOrderID}; got[0] != "s" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("並び: %v（売りが先、買い同士は元の順）", got)
	}
	if orders[0].ClientOrderID != "a" {
		t.Error("元の並びを書き換えた")
	}

	mgr := risk.NewRiskManager(wbjpcfg.RiskConfig{
		MaxOrderValue: dec("10000000"), MaxOrdersPerDay: 2, MaxDailyLoss: dec("1000000"),
		MaxPositionWeight: dec("1"), MaxGrossExposure: dec("10"),
	}, []string{"1001", "1002", "9984"})
	ctx := riskContext("10000000", map[string]decimal.Decimal{"1001": dec("1000"), "1002": dec("1000"), "9984": dec("1000")})
	ctx.Positions["9984"] = domain.Position{Symbol: "9984", Quantity: dec("100"), AvailableQuantity: dec("100"), CostPrice: dec("1200")}
	res, err := PlaceOrders(openRepo(t), &stubBroker{}, sorted, ctx, Options{RunID: "run-1", Live: true, Risk: mgr, Report: &recorder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, rejected := res.RiskRejected["9984"]; rejected || res.Placed != 2 {
		t.Errorf("損切りの売りが件数の上限で見送られた: %+v", res)
	}
	if !strings.Contains(res.RiskRejected["1002"], "発注件数") {
		t.Errorf("上限を超えた買いが見送られない: %+v", res)
	}
}

func TestPlaceOrdersSellDoesNotReserveBuyingPower(t *testing.T) {
	ctx := riskContext("1000", nil)
	limit := dec("1000")
	req, _ := domain.NewOrderRequest("s", "7203", domain.SideSell, domain.OrderTypeLimit,
		dec("100"), &limit, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	Reserve(ctx, req)
	if ctx.OrdersToday != 1 || !ctx.Balance.BuyingPower.Equal(dec("1000")) || len(ctx.PendingValue) != 0 {
		t.Errorf("売りで余力を動かした: %+v", ctx)
	}
	// 成行の買いは基準値段で見積もる。未約定の地図が nil でも落ちない
	mkt, _ := domain.NewOrderRequest("m", "7203", domain.SideBuy, domain.OrderTypeMarket,
		dec("100"), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	ctx = &risk.RiskContext{Balance: domain.Balance{BuyingPower: dec("100000")},
		BasePrices: map[string]decimal.Decimal{"7203": dec("500")}}
	Reserve(ctx, mkt)
	if !ctx.Balance.BuyingPower.Equal(dec("50000")) || !ctx.PendingValue["7203"].Equal(dec("50000")) {
		t.Errorf("成行の見積もり: %+v", ctx)
	}
}

func TestPlaceOrdersSkipsAlreadyPlaced(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{}
	req := buy(t, "a", "7203", 1000, 100)
	if err := PlaceRecorded(rep, b, "run-1", req, &recorder{}); err != nil {
		t.Fatal(err)
	}
	ctx := riskContext("10000000", map[string]decimal.Decimal{"7203": dec("1000")})
	res, err := PlaceOrders(rep, b, []domain.OrderRequest{req}, ctx,
		Options{RunID: "run-1", Live: true, Risk: riskManager("7203"), Report: &recorder{}})
	if err != nil || res.AlreadyPlaced != 1 || res.Placed != 0 || len(b.placed) != 1 {
		t.Errorf("発注済みを送り直した: %+v placed=%d err=%v", res, len(b.placed), err)
	}
}

func TestPlaceOrdersContinuesAfterRejectionAndStopsOnUnconfirmed(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{place: func(req domain.OrderRequest) (*domain.OrderAck, error) {
		switch req.Symbol {
		case "7203":
			return nil, &broker.OrderRejectedError{Message: "拒否"}
		case "6758":
			return nil, errors.New("timeout")
		}
		id := "N/" + req.ClientOrderID
		return &domain.OrderAck{BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
	}}
	prices := map[string]decimal.Decimal{"7203": dec("1000"), "6758": dec("1000"), "9984": dec("1000")}
	ctx := riskContext("10000000", prices)
	orders := []domain.OrderRequest{buy(t, "a", "7203", 1000, 100), buy(t, "b", "6758", 1000, 100), buy(t, "c", "9984", 1000, 100)}
	res, err := PlaceOrders(rep, b, orders, ctx, Options{RunID: "run-1", Live: true,
		Risk: riskManager("7203", "6758", "9984"), Report: &recorder{}})
	var unconfirmed *ErrUnconfirmedOrder
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("結果不明で止まっていない: %v", err)
	}
	if len(res.Failed) != 1 || len(b.placed) != 2 {
		t.Errorf("拒否の次へ進み、結果不明で止まるはず: failed=%v placed=%d", res.Failed, len(b.placed))
	}
	if ctx.OrdersToday != 0 || !ctx.Balance.BuyingPower.Equal(dec("10000000")) {
		t.Errorf("通っていない注文で余力を減らした: %+v", ctx)
	}
}

func TestPlaceOrdersStopsWhenLedgerUnreadable(t *testing.T) {
	rep := openRepo(t)
	_ = rep.Close()
	b := &stubBroker{}
	ctx := riskContext("10000000", map[string]decimal.Decimal{"7203": dec("1000")})
	_, err := PlaceOrders(rep, b, []domain.OrderRequest{buy(t, "a", "7203", 1000, 100)}, ctx,
		Options{RunID: "run-1", Live: true, Risk: riskManager("7203"), Report: &recorder{}})
	if err == nil || len(b.placed) != 0 {
		t.Errorf("台帳が読めないのに送った: err=%v placed=%d", err, len(b.placed))
	}
}

// --- SyncFills --------------------------------------------------------------

func todayJST() string {
	return clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02")
}

// TestSyncFillsRecordsFillsSoGuardsWork は、約定を取り込むと当日買付に載り
// （差金決済の柵が効く）、未約定の買いから外れること。
func TestSyncFillsRecordsFillsSoGuardsWork(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{}
	if err := PlaceRecorded(rep, b, "run-1", buy(t, "a", "7203", 1000, 100), &recorder{}); err != nil {
		t.Fatal(err)
	}
	if pending, _ := rep.PendingBuyValue(nil); !pending["7203"].Equal(dec("100000")) {
		t.Fatalf("前提: 未約定 %v", pending)
	}
	b.getOrder = func(id string, brokerID *string) (*domain.Order, error) {
		if brokerID == nil || *brokerID != "N/a" {
			t.Errorf("注文番号で引いていない: %v", brokerID)
		}
		price := dec("1010")
		return &domain.Order{ClientOrderID: id, BrokerOrderID: brokerID, Symbol: "7203",
			Quantity: dec("100"), FilledQuantity: dec("100"), AvgFillPrice: &price, Status: domain.OrderStatusFilled}, nil
	}
	res, err := SyncFills(rep, b)
	if err != nil || len(res.Changes) != 1 || res.Changes[0].After != domain.OrderStatusFilled {
		t.Fatalf("取り込めていない: %+v err=%v", res, err)
	}
	o := orderOf(t, rep, "a")
	if !o.FilledQuantity.Equal(dec("100")) || o.AvgFillPrice == nil || !o.AvgFillPrice.Equal(dec("1010")) {
		t.Errorf("約定が台帳に無い: %+v", o)
	}
	bought, _ := rep.BoughtToday(todayJST())
	if _, ok := bought["7203"]; !ok {
		t.Errorf("当日買付に載らない: %v", bought)
	}
	if pending, _ := rep.PendingBuyValue(nil); len(pending) != 0 {
		t.Errorf("約定済みが未約定に残る: %v", pending)
	}
	// 変化が無ければ書かない・照会済みの終了状態は二度と引かない
	b.queried = nil
	if res, _ := SyncFills(rep, b); len(res.Changes) != 0 || len(b.queried) != 0 {
		t.Errorf("終了した注文を照会した: %+v queried=%v", res, b.queried)
	}
}

func TestSyncFillsPartialAndUnresolved(t *testing.T) {
	rep := openRepo(t)
	b := &stubBroker{}
	for _, req := range []domain.OrderRequest{buy(t, "part", "7203", 1000, 200), buy(t, "err", "6758", 1000, 100), buy(t, "gone", "9984", 1000, 100)} {
		if err := PlaceRecorded(rep, b, "run-1", req, &recorder{}); err != nil {
			t.Fatal(err)
		}
	}
	// 注文番号の無い PENDING は照会しない（一覧の突き合わせに任せる）
	pending := buy(t, "pend", "8306", 1000, 100)
	if err := rep.RecordOrder("run-1", pending, string(domain.OrderStatusPending), nil); err != nil {
		t.Fatal(err)
	}

	b.getOrder = func(id string, brokerID *string) (*domain.Order, error) {
		switch id {
		case "part":
			return &domain.Order{ClientOrderID: id, Quantity: dec("200"), FilledQuantity: dec("100"), Status: domain.OrderStatusPartiallyFilled}, nil
		case "err":
			return nil, errors.New("timeout")
		}
		return nil, nil
	}
	res, err := SyncFills(rep, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 1 || len(res.Unresolved) != 2 {
		t.Errorf("changes=%+v unresolved=%v", res.Changes, res.Unresolved)
	}
	for _, id := range b.queried {
		if id == "pend" {
			t.Error("注文番号の無い PENDING を照会した")
		}
	}
	if o := orderOf(t, rep, "part"); o.Status != domain.OrderStatusPartiallyFilled || !o.FilledQuantity.Equal(dec("100")) {
		t.Errorf("部分約定: %+v", o)
	}
	if o := orderOf(t, rep, "gone"); o.Status != domain.OrderStatusSubmitted {
		t.Errorf("ブローカーが知らない注文を勝手に書き換えた: %s", o.Status)
	}
	// 部分約定は未確定のまま残り、残りの 100 株だけが未約定に数えられる
	if pv, _ := rep.PendingBuyValue(nil); !pv["7203"].Equal(dec("100000")) {
		t.Errorf("部分約定の残り: %v", pv)
	}

	// 応答の約定数量が欠けても（0）、記録済みの約定を減らさない
	b.getOrder = func(id string, _ *string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Quantity: dec("200"), FilledQuantity: decimal.Zero, Status: domain.OrderStatusExpired}, nil
	}
	if _, err := SyncFills(rep, b); err != nil {
		t.Fatal(err)
	}
	if o := orderOf(t, rep, "part"); o.Status != domain.OrderStatusExpired || !o.FilledQuantity.Equal(dec("100")) {
		t.Errorf("約定数量が減った: %+v", o)
	}
}

// --- CancelRecorded ---------------------------------------------------------

func TestCancelRecorded(t *testing.T) {
	setup := func(t *testing.T) (*repo.Repo, *stubBroker) {
		rep := openRepo(t)
		b := &stubBroker{}
		if err := PlaceRecorded(rep, b, "run-1", buy(t, "a", "7203", 1000, 200), &recorder{}); err != nil {
			t.Fatal(err)
		}
		if err := rep.UpdateOrder("a", domain.OrderStatusPartiallyFilled, dec("100"), nil, nil); err != nil {
			t.Fatal(err)
		}
		return rep, b
	}

	t.Run("照会で終了を確かめたらその状態", func(t *testing.T) {
		rep, b := setup(t)
		b.getOrder = func(id string, brokerID *string) (*domain.Order, error) {
			return &domain.Order{ClientOrderID: id, Quantity: dec("200"), FilledQuantity: dec("150"), Status: domain.OrderStatusCancelled}, nil
		}
		res, err := CancelRecorded(rep, b, "a")
		if err != nil || !res.Recorded || res.Deferred || res.Status != domain.OrderStatusCancelled {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusCancelled || !o.FilledQuantity.Equal(dec("150")) {
			t.Errorf("台帳: %+v", o)
		}
		if len(b.cancelled) != 1 {
			t.Error("取消を送っていない")
		}
	})

	// W4 の再現。以前は照会できないと確かめずに CANCELLED と書いていた。CANCELLED は終了状態
	// なので、取消が間に合わず約定していた場合にその約定が二度と取り込まれない
	t.Run("照会できなければ状態を確定させない", func(t *testing.T) {
		rep, b := setup(t)
		b.getOrder = func(string, *string) (*domain.Order, error) { return nil, errors.New("timeout") }
		res, err := CancelRecorded(rep, b, "a")
		if err != nil || !res.Deferred || !res.Unverified || res.QueryErr == nil || res.Status != domain.OrderStatusPartiallyFilled {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusPartiallyFilled || !o.FilledQuantity.Equal(dec("100")) {
			t.Errorf("確かめずに書き換えた: %+v", o)
		}
		// 次の約定同期で照会できれば、そこで確定する
		b.getOrder = func(id string, _ *string) (*domain.Order, error) {
			return &domain.Order{ClientOrderID: id, Quantity: dec("200"), FilledQuantity: dec("200"), Status: domain.OrderStatusFilled}, nil
		}
		if _, err := SyncFills(rep, b); err != nil {
			t.Fatal(err)
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusFilled || !o.FilledQuantity.Equal(dec("200")) {
			t.Errorf("取消が間に合わなかった約定を取り込めていない: %+v", o)
		}
	})

	t.Run("照会の結果が空でも確定させない", func(t *testing.T) {
		rep, b := setup(t)
		b.getOrder = func(string, *string) (*domain.Order, error) { return nil, nil }
		res, err := CancelRecorded(rep, b, "a")
		if err != nil || !res.Unverified {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusPartiallyFilled {
			t.Errorf("確かめずに書き換えた: %+v", o)
		}
	})

	t.Run("まだ板に残っていれば書き換えない", func(t *testing.T) {
		rep, b := setup(t)
		b.getOrder = func(id string, _ *string) (*domain.Order, error) {
			return &domain.Order{ClientOrderID: id, Quantity: dec("200"), FilledQuantity: dec("100"), Status: domain.OrderStatusPartiallyFilled}, nil
		}
		res, err := CancelRecorded(rep, b, "a")
		if err != nil || !res.Deferred {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusPartiallyFilled {
			t.Errorf("反映前に書き換えた: %s", o.Status)
		}
	})

	t.Run("取消に失敗したら台帳はそのまま", func(t *testing.T) {
		rep, b := setup(t)
		b.cancelErr = errors.New("down")
		if _, err := CancelRecorded(rep, b, "a"); err == nil {
			t.Fatal("失敗が伝わっていない")
		}
		if o := orderOf(t, rep, "a"); o.Status != domain.OrderStatusPartiallyFilled {
			t.Errorf("取消できていないのに書き換えた: %s", o.Status)
		}
	})

	t.Run("台帳に無い注文は送るだけ", func(t *testing.T) {
		rep, b := setup(t)
		res, err := CancelRecorded(rep, b, "いない")
		if err != nil || res.Recorded || len(b.cancelled) != 1 {
			t.Errorf("res=%+v err=%v cancelled=%v", res, err, b.cancelled)
		}
	})

	t.Run("dry-run と終了済みは送らない", func(t *testing.T) {
		rep, b := setup(t)
		if err := rep.RecordOrder("run-1", buy(t, "dry", "6758", 1000, 100), DryRunStatus, nil); err != nil {
			t.Fatal(err)
		}
		if err := rep.UpdateOrderStatus("a", domain.OrderStatusFilled); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"dry", "a"} {
			if _, err := CancelRecorded(rep, b, id); err == nil {
				t.Errorf("%s: 送るべきでない注文が通った", id)
			}
		}
		if len(b.cancelled) != 0 {
			t.Errorf("取消を送った: %v", b.cancelled)
		}
	})
}
