package execute

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// recordEntry は前日 (day-back) に建てて約定した建玉を台帳に残す。
func recordEntry(t *testing.T, env Env, back int, symbol string, side domain.Side, qty int64, price int64) (string, time.Time) {
	t.Helper()
	d := env.Day.AddDate(0, 0, -back)
	req, err := domain.NewOrderRequest("e-"+symbol+"-"+d.Format("0102"), symbol, side, domain.OrderTypeMarket,
		decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginOpen)
	if err != nil {
		t.Fatal(err)
	}
	id := "B/" + req.ClientOrderID
	if err := env.Ledger.Record(req, d, string(domain.OrderStatusSubmitted), nil, &id); err != nil {
		t.Fatal(err)
	}
	p := decimal.NewFromInt(price)
	if err := env.Ledger.UpdateStatus(req.ClientOrderID, domain.OrderStatusFilled, decimal.NewFromInt(qty), &p, &id); err != nil {
		t.Fatal(err)
	}
	return req.ClientOrderID, d
}

// recordDeadExit は前日の手仕舞いが失効した（約定 0）記録。
func recordDeadExit(t *testing.T, env Env, d time.Time, symbol string, side domain.Side, qty int64) string {
	t.Helper()
	req, err := domain.NewOrderRequest("x-"+symbol+"-"+d.Format("0102"), symbol, side, domain.OrderTypeMarket,
		decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginClose)
	if err != nil {
		t.Fatal(err)
	}
	id := "B/" + req.ClientOrderID
	if err := env.Ledger.Record(req, d, string(domain.OrderStatusExpired), nil, &id); err != nil {
		t.Fatal(err)
	}
	return req.ClientOrderID
}

func filledLookup(filled map[string]int64) func(string) (*domain.Order, error) {
	return func(clientOrderID string) (*domain.Order, error) {
		q, ok := filled[clientOrderID]
		if !ok {
			return nil, nil
		}
		status := domain.OrderStatusFilled
		if q == 0 {
			status = domain.OrderStatusExpired
		}
		price := decimal.NewFromInt(1000)
		return &domain.Order{ClientOrderID: clientOrderID, Status: status, FilledQuantity: decimal.NewFromInt(q), AvgFillPrice: &price}, nil
	}
}

func margin(symbol string, qty int64) domain.Position {
	return domain.Position{Symbol: symbol, Quantity: decimal.NewFromInt(qty), Trade: domain.TradeTypeMarginOpen}
}

// 前日の売建 300 株の返済が失効し、ブローカーに −300 株が残っている → 持ち越し 300 株。
func TestCarriedPositionsFindsUnclosedShort(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{margin("7203", -300)},
	}

	carried, unconfirmed, err := CarriedPositions(env, b)
	if err != nil || len(unconfirmed) != 0 {
		t.Fatalf("err %v, unconfirmed %v", err, unconfirmed)
	}
	if len(carried) != 1 {
		t.Fatalf("持ち越し 1 件のはず: %+v", carried)
	}
	c := carried[0]
	if c.Leg() != "short" || !c.Target.Quantity.Equal(decimal.NewFromInt(300)) || !c.Day.Equal(d) {
		t.Errorf("売建 300 株・建て日 %s のはず: %s", d.Format("2006-01-02"), c)
	}
	if !c.Notional().Equal(decimal.NewFromInt(300_000)) {
		t.Errorf("拘束 300,000 円のはず: %s", c.Notional())
	}
	long, short := TiedCapital(carried)
	if !long.IsZero() || !short.Equal(decimal.NewFromInt(300_000)) {
		t.Errorf("ショートの拘束 300,000 円のはず: %s / %s", long, short)
	}
}

// 手仕舞いが一部約定（300 のうち 200）なら残り 100 株。ブローカーの建玉で頭打ち。
func TestCarriedPositionsUsesRemainingAndBrokerHolding(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "9984", domain.SideBuy, 300, 2000)
	exitID := recordDeadExit(t, env, d, "9984", domain.SideSell, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 200}),
		positions: []domain.Position{margin("9984", 100)},
	}
	carried, _, err := CarriedPositions(env, b)
	if err != nil || len(carried) != 1 || !carried[0].Target.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("残り 100 株のはず: %v %v", carried, err)
	}

	// ブローカーに 40 株しか無ければ 40（手で一部返済済み）
	b.positions = []domain.Position{margin("9984", 40)}
	carried, _, _ = CarriedPositions(env, b)
	if len(carried) != 1 || !carried[0].Target.Quantity.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("ブローカーの 40 株で頭打ちのはず: %v", carried)
	}

	// ブローカーに無ければ対象外（手で返済済み）
	b.positions = nil
	if carried, _, _ = CarriedPositions(env, b); len(carried) != 0 {
		t.Fatalf("建玉が無ければ持ち越し無しのはず: %v", carried)
	}
}

// 照会できない建玉は数量を推測せず、unconfirmed に積んで対象にしない。
func TestCarriedPositionsSkipsUnconfirmed(t *testing.T) {
	env, _ := newEnv(t)
	d := env.Day.AddDate(0, 0, -1)
	req, _ := domain.NewOrderRequest("e-unknown", "6758", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginOpen)
	if err := env.Ledger.Record(req, d, string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}
	b := &stubBroker{positions: []domain.Position{margin("6758", 100)}}
	carried, unconfirmed, err := CarriedPositions(env, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 0 || len(unconfirmed) != 1 {
		t.Fatalf("照会できない建玉は対象外・unconfirmed 1 件のはず: %v / %v", carried, unconfirmed)
	}
}

// 返済は建てた日の下に記録され、再実行は冪等。今日の手仕舞い対象には混ざらない。
func TestReturnCarriedRecordsUnderEntryDayAndIsIdempotent(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{margin("7203", -300)},
		balance:   richBalance(),
	}
	carried, _, _ := CarriedPositions(env, b)
	if failures := ReturnCarried(env, b, carried, "翌寄りで持ち越しを手仕舞い"); len(failures) != 0 {
		t.Fatalf("返済が通るはず: %v", failures)
	}
	if len(b.placed) != 1 {
		t.Fatalf("返済 1 本のはず: %d", len(b.placed))
	}
	req := b.placed[0]
	if req.Side != domain.SideBuy || req.Trade != domain.TradeTypeMarginClose || !req.Quantity.Equal(decimal.NewFromInt(300)) {
		t.Errorf("返済買い 300 株のはず: %+v", req)
	}
	exits, _ := env.Ledger.ExitsOn(d)
	live := 0
	for _, o := range exits {
		if !o.IsDead() {
			live++
		}
	}
	if live != 1 {
		t.Errorf("建てた日（%s）の下に生きている手仕舞いが 1 本のはず: %d", d.Format("2006-01-02"), live)
	}
	if today, _ := env.Ledger.ExitsOn(env.Day); len(today) != 0 {
		t.Errorf("今日の台帳には積まない: %d", len(today))
	}

	// 2 回目（再実行）: 生きている返済があるので何も送らない
	ReturnCarried(env, b, carried, "翌寄りで持ち越しを手仕舞い")
	if len(b.placed) != 1 {
		t.Errorf("再実行は冪等のはず: %d 本", len(b.placed))
	}
}

func TestPositionsWithin(t *testing.T) {
	cases := []struct {
		capital, tied, budget int64
		want                  int
	}{
		{3_000_000, 0, 1_000_000, 3},
		{3_000_000, 1_000_000, 1_000_000, 2},
		{3_000_000, 2_500_000, 1_000_000, 0},
		{3_000_000, 4_000_000, 1_000_000, 0},
		{2_000_000, 300_000, 666_666, 2},
	}
	for _, c := range cases {
		got := PositionsWithin(decimal.NewFromInt(c.capital), decimal.NewFromInt(c.tied), decimal.NewFromInt(c.budget))
		if got != c.want {
			t.Errorf("PositionsWithin(%d, %d, %d) = %d, want %d", c.capital, c.tied, c.budget, got, c.want)
		}
	}
}

// 持ち越した銘柄が今日も候補でも、台帳外の建玉とは見なさない。
func TestEnsureNoUnrecordedPositionsAllowsCarried(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{margin("7203", -300)},
	}
	carried, _, _ := CarriedPositions(env, b)
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	if err := EnsureNoUnrecordedPositions(env, b, picks, carried); err != nil {
		t.Fatalf("持ち越しは既知の建玉のはず: %v", err)
	}
	if err := EnsureNoUnrecordedPositions(env, b, picks, nil); err == nil {
		t.Fatal("持ち越しを知らなければ台帳外として止まるはず")
	}
}
