package execute

import (
	"errors"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
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

// recordDeadExit は前日の手仕舞いが失効した（約定 0、台帳で確定済み）記録。
func recordDeadExit(t *testing.T, env Env, d time.Time, symbol string, side domain.Side, qty int64) string {
	t.Helper()
	return recordExit(t, env, d, symbol, side, qty, domain.OrderStatusExpired)
}

// recordOpenExit は前日の手仕舞いが台帳で未確定のまま（送信済み。照会が要る）の記録。
func recordOpenExit(t *testing.T, env Env, d time.Time, symbol string, side domain.Side, qty int64) string {
	t.Helper()
	return recordExit(t, env, d, symbol, side, qty, domain.OrderStatusSubmitted)
}

func recordExit(t *testing.T, env Env, d time.Time, symbol string, side domain.Side, qty int64, status domain.OrderStatus) string {
	t.Helper()
	req, err := domain.NewOrderRequest("x-"+symbol+"-"+d.Format("0102"), symbol, side, domain.OrderTypeMarket,
		decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginClose)
	if err != nil {
		t.Fatal(err)
	}
	id := "B/" + req.ClientOrderID
	if err := env.Ledger.Record(req, d, string(status), nil, &id); err != nil {
		t.Fatal(err)
	}
	return req.ClientOrderID
}

// countingLookup は filledLookup に照会回数の記録を足す。
func countingLookup(filled map[string]int64, calls *int) func(string) (*domain.Order, error) {
	inner := filledLookup(filled)
	return func(clientOrderID string) (*domain.Order, error) {
		*calls++
		return inner(clientOrderID)
	}
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

	carried, unconfirmed, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
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
// 手仕舞いは台帳で未確定（verify が走らなかった）なので、照会して約定を知る。
func TestCarriedPositionsUsesRemainingAndBrokerHolding(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "9984", domain.SideBuy, 300, 2000)
	exitID := recordOpenExit(t, env, d, "9984", domain.SideSell, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 200}),
		positions: []domain.Position{margin("9984", 100)},
	}
	carried, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
	if err != nil || len(carried) != 1 || !carried[0].Target.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("残り 100 株のはず: %v %v", carried, err)
	}

	// ブローカーに 40 株しか無ければ 40（手で一部返済済み）
	b.positions = []domain.Position{margin("9984", 40)}
	carried, _, _ = CarriedPositions(env, b, broker.PositionsByLeg(b))
	if len(carried) != 1 || !carried[0].Target.Quantity.Equal(decimal.NewFromInt(40)) {
		t.Fatalf("ブローカーの 40 株で頭打ちのはず: %v", carried)
	}

	// ブローカーに無ければ対象外（手で返済済み）
	b.positions = nil
	if carried, _, _ = CarriedPositions(env, b, broker.PositionsByLeg(b)); len(carried) != 0 {
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
	carried, unconfirmed, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
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
	carried, _, _ := CarriedPositions(env, b, broker.PositionsByLeg(b))
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

// 台帳で確定済みの注文（約定・失効）はブローカーに聞かない。14 暦日ぶんを open / close の
// たびに聞くと 1 実行で百を超える電文になる。未確定の注文だけ聞く。
func TestCarriedPositionsQueriesOnlyOpenOrders(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	calls := 0
	b := &stubBroker{
		getOrder:  countingLookup(map[string]int64{entryID: 300, exitID: 0}, &calls),
		positions: []domain.Position{margin("7203", -300)},
	}
	carried, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
	if err != nil || len(carried) != 1 {
		t.Fatalf("持ち越し 1 件のはず: %v %v", carried, err)
	}
	if calls != 0 {
		t.Errorf("確定済みの注文をブローカーに聞いている: %d 回", calls)
	}

	// 未確定の手仕舞いが 1 本あれば、それだけ聞く
	openID := recordOpenExit(t, env, d, "6758", domain.SideBuy, 100)
	recordEntry(t, env, 1, "6758", domain.SideSell, 100, 1000)
	b.getOrder = countingLookup(map[string]int64{openID: 100}, &calls)
	if _, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("未確定の 1 本だけを聞くはず: %d 回", calls)
	}
	// 照会の結果は台帳に残る（次の実行はもう聞かない）
	exits, _ := env.Ledger.ExitsOn(d)
	for _, o := range exits {
		if o.ClientOrderID == openID && o.IsOpen() {
			t.Error("照会した約定が台帳に残っていない")
		}
	}
}

// 同じ脚の持ち越しが複数日にあれば、ブローカーの建玉を日をまたいで消費する。
// 日ごとに建玉全体を上限にすると、返済の合計が建玉（手で一部返済済み）を超える。
func TestCarriedPositionsConsumesHoldingAcrossDays(t *testing.T) {
	env, _ := newEnv(t)
	e1, d1 := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	x1 := recordDeadExit(t, env, d1, "7203", domain.SideBuy, 300)
	e2, d2 := recordEntry(t, env, 2, "7203", domain.SideSell, 300, 1000)
	x2 := recordDeadExit(t, env, d2, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder:  filledLookup(map[string]int64{e1: 300, x1: 0, e2: 300, x2: 0}),
		positions: []domain.Position{margin("7203", -400)}, // 600 のうち 200 は手で返済済み
	}
	carried, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
	if err != nil {
		t.Fatal(err)
	}
	total := decimal.Zero
	for _, c := range carried {
		total = total.Add(c.Target.Quantity)
	}
	if len(carried) != 2 || !total.Equal(decimal.NewFromInt(400)) {
		t.Fatalf("返済の合計は建玉の 400 株で頭打ちのはず: %v", carried)
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
	carried, _, _ := CarriedPositions(env, b, broker.PositionsByLeg(b))
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	if err := EnsureNoUnrecordedPositions(env, b, picks, carried); err != nil {
		t.Fatalf("持ち越しは既知の建玉のはず: %v", err)
	}
	if err := EnsureNoUnrecordedPositions(env, b, picks, nil); err == nil {
		t.Fatal("持ち越しを知らなければ台帳外として止まるはず")
	}
}

func TestCapByTied(t *testing.T) {
	d := decimal.NewFromInt
	cases := []struct {
		name                  string
		n, placed             int
		capital, tied, budget int64
		wantN                 int
		wantBudget            int64
	}{
		{"拘束なし", 3, 0, 3_000_000, 0, 1_000_000, 3, 1_000_000},
		{"1 件ぶん拘束 → 2 件", 3, 0, 3_000_000, 1_000_000, 1_000_000, 2, 1_000_000},
		{"2 件半拘束 → 残り 50 万で 1 件を小さく", 3, 0, 3_000_000, 2_500_000, 1_000_000, 1, 500_000},
		{"全額拘束 → 0 件", 3, 0, 3_000_000, 3_000_000, 1_000_000, 0, 1_000_000},
		{"拘束が上限を超える → 0 件", 3, 0, 3_000_000, 4_000_000, 1_000_000, 0, 1_000_000},
		{"縮小後の予算でも同じ規則", 3, 0, 3_000_000, 1_000_000, 500_000, 3, 500_000},
		{"もともと 0 件なら 0", 0, 0, 3_000_000, 0, 1_000_000, 0, 1_000_000},
		// 再実行: 前の回が建てた分の資金も引く。1 件ぶん拘束で 2 件建て済みなら残り 0
		{"再実行で資金を使い切っている → 0 件", 1, 2, 3_000_000, 1_000_000, 1_000_000, 0, 1_000_000},
		{"再実行で残りが 1 件ぶん → 1 件", 2, 1, 3_000_000, 1_000_000, 1_000_000, 1, 1_000_000},
		{"再実行で残りが予算未満 → 1 件を小さく", 2, 1, 3_000_000, 1_500_000, 1_000_000, 1, 500_000},
	}
	for _, c := range cases {
		n, budget := CapByTied(c.n, c.placed, d(c.capital), d(c.tied), d(c.budget))
		if n != c.wantN || !budget.Equal(d(c.wantBudget)) {
			t.Errorf("%s: (%d, %s), want (%d, %d)", c.name, n, budget, c.wantN, c.wantBudget)
		}
	}
}

// 積立が現物で持っている銘柄をデイトレが売建てて持ち越しても、相殺されて見失わない。
// 口座は共用（台帳は別）なので、銘柄コードだけで数えると 300 − 300 = 0 になる。
func TestCarriedPositionsIgnoresCashHolding(t *testing.T) {
	env, _ := newEnv(t)
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	b := &stubBroker{
		getOrder: filledLookup(map[string]int64{entryID: 300, exitID: 0}),
		positions: []domain.Position{
			{Symbol: "7203", Quantity: decimal.NewFromInt(300), Trade: domain.TradeTypeCash},
			margin("7203", -300),
		},
	}
	carried, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 1 || !carried[0].Target.Quantity.Equal(decimal.NewFromInt(300)) {
		t.Fatalf("売建 300 株の持ち越しを拾えていない: %+v", carried)
	}
}

// 積立の現物は「台帳外の建玉」に数えない（数えるとその日の発注が丸ごと止まる）。
func TestEnsureNoUnrecordedPositionsIgnoresCashHolding(t *testing.T) {
	env, _ := newEnv(t)
	env.Cfg.Margin.Enabled = true
	env.Cfg.Margin.LongViaMargin = true // ロングも信用 → 現物は積立のものでしかない
	b := &stubBroker{positions: []domain.Position{
		{Symbol: "7203", Quantity: decimal.NewFromInt(300), Trade: domain.TradeTypeCash},
	}}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	if err := EnsureNoUnrecordedPositions(env, b, picks, nil); err != nil {
		t.Errorf("積立の現物で発注が止まった: %v", err)
	}

	// 同じ銘柄に台帳外の信用建玉があれば止める
	b.positions = append(b.positions, margin("7203", 100))
	if err := EnsureNoUnrecordedPositions(env, b, picks, nil); err == nil {
		t.Error("台帳外の信用建玉で止まらない")
	}
}

// 台帳に無い信用建玉は返済の対象にする。現物は積立のものかもしれないので触らない。
func TestUnrecordedMarginSweepsOnlyMargin(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{positions: []domain.Position{
		{Symbol: "7203", Quantity: decimal.NewFromInt(300), Trade: domain.TradeTypeCash, CostPrice: decimal.NewFromInt(2000)},
		{Symbol: "6758", Quantity: decimal.NewFromInt(-200), Trade: domain.TradeTypeMarginOpen, CostPrice: decimal.NewFromInt(1500)},
		{Symbol: "9984", Quantity: decimal.NewFromInt(100), Trade: domain.TradeTypeMarginOpen, CostPrice: decimal.NewFromInt(8000)},
	}}
	out, err := UnrecordedMargin(env, broker.PositionsByLeg(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("信用の 2 件だけを返済するはず: %+v", out)
	}
	if out[0].Target.Entry.Symbol != "6758" || out[0].Leg() != "short" {
		t.Errorf("売建 200 株が対象になっていない: %s", out[0])
	}
	if !out[0].Notional().Equal(decimal.NewFromInt(300000)) {
		t.Errorf("拘束金額が建値で出ていない: %s", out[0].Notional())
	}
	if out[1].Target.Entry.Symbol != "9984" || out[1].Leg() != "long" {
		t.Errorf("信用買い 100 株が対象になっていない: %s", out[1])
	}
	for _, c := range out {
		if !c.Target.Unrecorded {
			t.Errorf("台帳外の印が付いていない: %s", c)
		}
	}
	swept := SweptSymbols(out)
	if _, ok := swept["6758"]; !ok || len(swept) != 2 {
		t.Errorf("返済に回した 2 銘柄が候補の除外に入っていない: %v", swept)
	}
}

// 台帳が知っている建玉（今日の建玉・判定済みの持ち越し）は差し引く。二重に返済しない。
func TestUnrecordedMarginSubtractsLedger(t *testing.T) {
	env, _ := newEnv(t)
	env.Cfg.Margin.Enabled = true
	env.Cfg.Margin.LongViaMargin = true
	b := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := LiveEntries(env)
	quantity := entries[0].Quantity
	b.positions = []domain.Position{
		{Symbol: "7203", Quantity: quantity, Trade: domain.TradeTypeMarginOpen, CostPrice: decimal.NewFromInt(1000)},
	}
	out, err := UnrecordedMargin(env, broker.PositionsByLeg(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("今日の建玉を台帳外として返済しようとした: %+v", out)
	}

	// ブローカーの建玉が台帳より多ければ、超えたぶんだけ返済する
	b.positions[0].Quantity = quantity.Add(decimal.NewFromInt(100))
	out, err = UnrecordedMargin(env, broker.PositionsByLeg(b), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !out[0].Target.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("超過 100 株だけを返済するはず: %+v", out)
	}
}

// 台帳外の返済は、同じ銘柄・同じ株数の通常の手仕舞いと注文 ID が衝突しない。
func TestUnrecordedExitIDDiffersFromNormalExit(t *testing.T) {
	entry := ledger.Order{Symbol: "7203", Side: domain.SideBuy, Trade: domain.TradeTypeMarginOpen}
	qty := decimal.NewFromInt(100)
	cfg := config.Default()
	normal, _ := ExitRequestAs(ExitTarget{Entry: entry, Quantity: qty}, day, cfg, 0, "引けで手仕舞い")
	sweep, _ := ExitRequestAs(ExitTarget{Entry: entry, Quantity: qty, Unrecorded: true}, day, cfg, 0, "台帳外を返済")
	if normal.ClientOrderID == sweep.ClientOrderID {
		t.Fatalf("注文 ID が衝突している: %s", normal.ClientOrderID)
	}
}

// 現物と信用は別の電文。片方が落ちても、もう片方だけを見る判断は止めない。
func TestPositionQueryFailuresAreIndependent(t *testing.T) {
	env, _ := newEnv(t)
	env.Cfg.Margin.Enabled = true
	env.Cfg.Margin.LongViaMargin = true // 信用でしか建てない構成
	entryID, d := recordEntry(t, env, 1, "7203", domain.SideSell, 300, 1000)
	exitID := recordDeadExit(t, env, d, "7203", domain.SideBuy, 300)
	newBroker := func() *stubBroker {
		return &stubBroker{
			getOrder:  filledLookup(map[string]int64{entryID: 300, exitID: 0}),
			positions: []domain.Position{margin("7203", -300)},
		}
	}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}

	// 現物が落ちても、信用しか見ないデイトレの判断は続く
	b := newBroker()
	b.posErr = errors.New("現物が取れない")
	carried, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b))
	if err != nil {
		t.Fatalf("現物の障害で持ち越しの判定が止まった: %v", err)
	}
	if len(carried) != 1 {
		t.Fatalf("売建の持ち越しを拾えていない: %+v", carried)
	}
	if _, err := UnrecordedMargin(env, broker.PositionsByLeg(b), carried); err != nil {
		t.Errorf("現物の障害で台帳外の判定が止まった: %v", err)
	}
	if err := EnsureNoUnrecordedPositions(env, b, picks, carried); err != nil {
		t.Errorf("現物の障害で発注が止まった: %v", err)
	}

	// 信用が落ちたときは止める（持ち越しを 0 株と読むと見失う）
	b = newBroker()
	b.marginErr = errors.New("信用が取れない")
	if _, _, err := CarriedPositions(env, b, broker.PositionsByLeg(b)); err == nil {
		t.Error("信用の障害で持ち越しの判定が止まらない")
	}
	if _, err := UnrecordedMargin(env, broker.PositionsByLeg(b), nil); err == nil {
		t.Error("信用の障害で台帳外の判定が止まらない")
	}
	if err := EnsureNoUnrecordedPositions(env, b, picks, nil); err == nil {
		t.Error("信用の障害で発注が止まらない")
	}
}
