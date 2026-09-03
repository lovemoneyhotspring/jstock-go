package execute

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/shopspring/decimal"
)

// stubBroker は発注の規則を検証するための模型。応答を差し替えられる。
type stubBroker struct {
	place      func(domain.OrderRequest) (*domain.OrderAck, error)
	getOrder   func(clientOrderID string) (*domain.Order, error)
	positions  []domain.Position
	posErr     error
	balance    domain.Balance
	placed     []domain.OrderRequest
	history    []domain.Order // 当日の注文一覧
	historyErr error
}

func (s *stubBroker) Name() string      { return "stub" }
func (s *stubBroker) AccountID() string { return "stub" }
func (s *stubBroker) GetBalance() (*domain.Balance, error) {
	b := s.balance
	return &b, nil
}
func (s *stubBroker) GetPositions() ([]domain.Position, error) { return s.positions, s.posErr }
func (s *stubBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	if s.posErr != nil {
		return nil, s.posErr
	}
	return broker.PositionsBySymbolHelper(s.positions), nil
}
func (s *stubBroker) GetOpenOrders() ([]domain.Order, error) { return nil, nil }
func (s *stubBroker) GetOrder(clientOrderID string, _ *string) (*domain.Order, error) {
	if s.getOrder == nil {
		return nil, nil
	}
	return s.getOrder(clientOrderID)
}
func (s *stubBroker) GetOrderHistory(_, _ time.Time) ([]domain.Order, error) {
	return s.history, s.historyErr
}
func (s *stubBroker) Preview(_ domain.OrderRequest) (*domain.OrderPreview, error) {
	return &domain.OrderPreview{}, nil
}
func (s *stubBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.placed = append(s.placed, req)
	if s.place == nil {
		id := "N/" + req.Symbol
		return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
	}
	return s.place(req)
}
func (s *stubBroker) Cancel(_ string, _ *string) error { return nil }
func (s *stubBroker) LotSizes(_ []string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{}
}

// recorder は Reporter の模型。通知と Error ログを覚える。
type recorder struct {
	alerts []string
	errors []string
}

func (r *recorder) Info(string, string, ...map[string]any) {}
func (r *recorder) Warn(string, string, ...map[string]any) {}
func (r *recorder) Error(code, msg string, _ ...map[string]any) {
	r.errors = append(r.errors, code+": "+msg)
}
func (r *recorder) Alert(title, body string) { r.alerts = append(r.alerts, title+" | "+body) }

var day = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

func newEnv(t *testing.T) (Env, *recorder) {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "daytrade-test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { led.Close() })
	t.Cleanup(execution.Reset)
	rep := &recorder{}
	return Env{Cfg: config.Default(), Ledger: led, Day: day, Report: rep, Out: &strings.Builder{}}, rep
}

func pick(symbol string, side domain.Side) selection.Pick {
	return selection.Pick{
		Symbol: symbol, Side: side, Rank: 1,
		PrevClose: decimal.NewFromInt(1030), Price: decimal.NewFromInt(1000),
		Gap: decimal.RequireFromString("-0.03"), Quantity: decimal.NewFromInt(100),
	}
}

func richBalance() domain.Balance {
	margin := decimal.NewFromInt(10_000_000)
	return domain.Balance{BuyingPower: decimal.NewFromInt(10_000_000), MarginBuyingPower: &margin}
}

func statusOf(t *testing.T, env Env, symbol string) ledger.Order {
	t.Helper()
	entries, err := env.Ledger.EntriesOn(day)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range entries {
		if o.Symbol == symbol {
			return o
		}
	}
	t.Fatalf("%s が台帳にない", symbol)
	return ledger.Order{}
}

func TestPlacePicksDryRunRecordsOnly(t *testing.T) {
	env, _ := newEnv(t)
	orders, failures, err := PlacePicks(env, nil, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || orders != 1 || len(failures) != 0 {
		t.Fatalf("orders=%d failures=%v err=%v", orders, failures, err)
	}
	if o := statusOf(t, env, "7203"); !o.IsDryRun() {
		t.Errorf("dry-run が台帳に残っていない: %s", o.Status)
	}
}

func TestPlacePicksLiveIsIdempotent(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}

	orders, failures, err := PlacePicks(env, b, picks)
	if err != nil || orders != 1 || len(failures) != 0 {
		t.Fatalf("orders=%d failures=%v err=%v", orders, failures, err)
	}
	o := statusOf(t, env, "7203")
	if o.Status != string(domain.OrderStatusSubmitted) || o.BrokerOrderID == nil || *o.BrokerOrderID != "N/7203" {
		t.Errorf("受理が台帳に反映されていない: %+v", o)
	}
	if o.Trade != domain.TradeTypeCash {
		t.Errorf("既定は現物: %s", o.Trade)
	}

	// 同じ日にもう一度走っても送り直さない
	orders, _, _ = PlacePicks(env, b, picks)
	if orders != 0 || len(b.placed) != 1 {
		t.Errorf("二重発注: orders=%d placed=%d", orders, len(b.placed))
	}
}

func TestPlacePicksRejectedIsResentWithNewSeed(t *testing.T) {
	env, rep := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, &broker.OrderRejectedError{Message: "11029"}
	}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	orders, failures, _ := PlacePicks(env, b, picks)
	if orders != 0 || len(failures) != 1 {
		t.Fatalf("orders=%d failures=%v", orders, failures)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusRejected) {
		t.Errorf("拒否が REJECTED になっていない: %s", o.Status)
	}
	if len(rep.errors) != 0 {
		t.Errorf("正常に記録できたのに Error ログ: %v", rep.errors)
	}

	// 次の実行は種を変えて送り直す（同じ ID はブローカーが弾く）
	b.place = nil
	orders, _, _ = PlacePicks(env, b, picks)
	if orders != 1 || len(b.placed) != 2 || b.placed[0].ClientOrderID == b.placed[1].ClientOrderID {
		t.Errorf("拒否後の再送: orders=%d placed=%v", orders, b.placed)
	}
}

func TestPlacePicksUnconfirmedStaysPending(t *testing.T) {
	env, _ := newEnv(t)
	// 一覧も照会できない → 届いたかどうか決められない → PENDING のまま、送り直さない
	b := &stubBroker{balance: richBalance(), historyErr: errors.New("down")}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, errors.New("timeout")
	}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	_, failures, _ := PlacePicks(env, b, picks)
	if len(failures) != 1 || !strings.Contains(failures[0], "確認できません") {
		t.Fatalf("結果不明が伝わっていない: %v", failures)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusPending) {
		t.Errorf("結果不明は PENDING のまま: %s", o.Status)
	}
	// 届いていたかもしれないので、次の実行は送り直さない（二重買付より買い漏れ）
	b.place = nil
	orders, _, _ := PlacePicks(env, b, picks)
	if orders != 0 || len(b.placed) != 1 {
		t.Errorf("結果不明の注文が送り直された: placed=%d", len(b.placed))
	}
}

func TestPlacePicksInsufficientFunds(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: domain.Balance{BuyingPower: decimal.NewFromInt(50_000)}}
	_, failures, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if len(failures) != 1 || len(b.placed) != 0 {
		t.Errorf("余力不足で送っている: failures=%v placed=%d", failures, len(b.placed))
	}
}

func TestEntryRequestUsesMarginWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Margin.LongViaMargin = true
	long := EntryRequest(pick("7203", domain.SideBuy), day, cfg, 0)
	short := EntryRequest(pick("9984", domain.SideSell), day, cfg, 0)
	if long.Trade != domain.TradeTypeMarginOpen || short.Trade != domain.TradeTypeMarginOpen {
		t.Errorf("信用: long=%s short=%s", long.Trade, short.Trade)
	}
	if EntryRequest(pick("7203", domain.SideBuy), day, config.Default(), 0).Trade != domain.TradeTypeCash {
		t.Error("既定は現物")
	}
}

func TestEnsureNoUnrecordedPositions(t *testing.T) {
	env, rep := newEnv(t)
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	b := &stubBroker{positions: []domain.Position{{Symbol: "7203", Quantity: decimal.NewFromInt(100)}}}

	err := EnsureNoUnrecordedPositions(env, b, picks)
	var unrecorded *ErrUnrecordedPositions
	if !errors.As(err, &unrecorded) || len(unrecorded.Positions) != 1 {
		t.Fatalf("台帳外の建玉で止まらない: %v", err)
	}
	if len(rep.alerts) != 1 {
		t.Errorf("通知が出ていない: %v", rep.alerts)
	}

	// 台帳が今日の建玉として知っていれば止めない（正常な再実行）
	req := EntryRequest(picks[0], day, env.Cfg, 0)
	if err := env.Ledger.Record(req, day, string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoUnrecordedPositions(env, b, picks); err != nil {
		t.Errorf("台帳が知っている建玉で止まった: %v", err)
	}

	// 照会できないときも止める
	b.posErr = errors.New("down")
	if err := EnsureNoUnrecordedPositions(env, b, picks); err == nil {
		t.Error("照会できないのに発注に進んだ")
	}
}

func TestExitRequestTradeTypes(t *testing.T) {
	qty := decimal.NewFromInt(100)
	cfg := config.Default()
	cases := []struct {
		entry      ledger.Order
		wantSide   domain.Side
		wantTrade  domain.TradeType
		wantAction string
	}{
		{ledger.Order{Symbol: "1", Side: domain.SideBuy, Trade: domain.TradeTypeCash}, domain.SideSell, domain.TradeTypeCash, "売り"},
		{ledger.Order{Symbol: "2", Side: domain.SideBuy, Trade: domain.TradeTypeMarginOpen}, domain.SideSell, domain.TradeTypeMarginClose, "返済売り"},
		{ledger.Order{Symbol: "3", Side: domain.SideSell, Trade: domain.TradeTypeMarginOpen}, domain.SideBuy, domain.TradeTypeMarginClose, "返済買い"},
	}
	for _, c := range cases {
		req, action := ExitRequest(c.entry, qty, day, cfg, 0)
		if req.Side != c.wantSide || req.Trade != c.wantTrade || action != c.wantAction {
			t.Errorf("%s: got %s/%s %q, want %s/%s %q", c.entry.Symbol, req.Side, req.Trade, action, c.wantSide, c.wantTrade, c.wantAction)
		}
	}
}

func TestRefreshEntriesAndPlaceExits(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	price := decimal.NewFromInt(990)
	b.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
			Quantity: decimal.NewFromInt(100), FilledQuantity: decimal.NewFromInt(100), AvgFillPrice: &price}, nil
	}
	entries, _, err := LiveEntries(env)
	if err != nil || len(entries) != 1 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 0 || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("targets=%+v unconfirmed=%v err=%v", targets, unconfirmed, err)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusFilled) || !o.FilledQuantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("約定が台帳に反映されていない: %+v", o)
	}

	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatalf("手仕舞い: %v", failures)
	}
	exit := b.placed[len(b.placed)-1]
	if exit.Side != domain.SideSell || exit.Trade != domain.TradeTypeCash || !exit.Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("手仕舞い注文: %+v", exit)
	}

	// 2 回目は「手仕舞い発注済み（冪等）」で対象なし
	targets, _, _ = RefreshEntries(env, b, entries)
	if len(targets) != 0 {
		t.Errorf("手仕舞いが二重に対象になった: %+v", targets)
	}
}

func TestRefreshEntriesUnconfirmedWhenOrderUnknown(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	// 照会が空（建ったかどうか分からない）→ 数量を推測して売らない
	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, _ := RefreshEntries(env, b, entries)
	if len(targets) != 0 || len(unconfirmed) != 1 {
		t.Errorf("targets=%+v unconfirmed=%v", targets, unconfirmed)
	}
	// dry-run（b なし）は送信済みを全約定とみなして対象を示す
	targets, _, _ = RefreshEntries(env, nil, entries)
	if len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("dry-run の対象: %+v", targets)
	}
}

func TestVerifyDetectsCarriedAndMismatch(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	filled := func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
			Quantity: decimal.NewFromInt(100), FilledQuantity: decimal.NewFromInt(100)}, nil
	}
	b.getOrder = filled

	// 建玉あり・手仕舞いなし → 持ち越し
	entries, exits, _ := LiveOrders(env)
	result := Verify(env, b, entries, exits)
	if len(result.Carried) != 1 || len(result.Unconfirmed) != 0 {
		t.Fatalf("持ち越しを検出できない: %+v", result)
	}

	// 手仕舞いも約定 → 持ち越しなし
	targets, _, _ := RefreshEntries(env, b, entries)
	PlaceExits(env, b, targets)
	entries, exits, _ = LiveOrders(env)
	result = Verify(env, b, entries, exits)
	if len(result.Carried) != 0 {
		t.Errorf("手仕舞い済みなのに持ち越し: %+v", result)
	}

	// 台帳では手仕舞い済みだがブローカーに建玉が残っている → 不一致として知らせる
	b.positions = []domain.Position{{Symbol: "7203", Quantity: decimal.NewFromInt(100)}}
	result = Verify(env, b, entries, exits)
	if len(result.Carried) != 1 || !strings.Contains(result.Carried[0], "不一致") {
		t.Errorf("台帳との不一致を検出できない: %+v", result)
	}

	// 照会できない注文があれば「持ち越しなし」と言わない
	b.positions = nil
	b.getOrder = func(string) (*domain.Order, error) { return nil, errors.New("down") }
	if err := env.Ledger.UpdateStatus(entries[0].ClientOrderID, domain.OrderStatusSubmitted, decimal.Zero, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, exits, _ = LiveOrders(env)
	result = Verify(env, b, entries, exits)
	if len(result.Unconfirmed) == 0 {
		t.Errorf("照会できない注文が数えられていない: %+v", result)
	}
}

// --- 送信結果不明（PENDING）の自動判定 ---

func brokerOrderOf(id, symbol string, side domain.Side, qty int64, trade domain.TradeType, status domain.OrderStatus) domain.Order {
	created := time.Now().UTC()
	return domain.Order{ClientOrderID: id, BrokerOrderID: &id, Symbol: symbol, Side: side, Trade: trade,
		Quantity: decimal.NewFromInt(qty), FilledQuantity: decimal.NewFromInt(qty), Status: status, CreatedAt: &created}
}

func todayEnv(t *testing.T) (Env, *recorder) {
	t.Helper()
	env, rep := newEnv(t)
	// 立花の一覧は当日分しか無いので、判定は「今日（JST）」の台帳にだけ効く
	now := time.Now().In(time.FixedZone("JST", 9*3600))
	env.Day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return env, rep
}

func TestUnconfirmedIsResentWhenBrokerHasNoOrder(t *testing.T) {
	env, _ := todayEnv(t)
	b := &stubBroker{balance: richBalance()}
	calls := 0
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("timeout") // 1 回目は結果不明
		}
		id := "N/" + req.Symbol
		return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
	}
	// 一覧に無い → 届いていない → 種を変えて同じ実行の中で送り直す
	orders, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || orders != 1 || len(failures) != 0 {
		t.Fatalf("orders=%d failures=%v err=%v", orders, failures, err)
	}
	if len(b.placed) != 2 || b.placed[0].ClientOrderID == b.placed[1].ClientOrderID {
		t.Fatalf("送り直しの ID: %+v", b.placed)
	}
	entries, _ := env.Ledger.EntriesOn(env.Day)
	statuses := map[string]string{}
	for _, o := range entries {
		statuses[o.ClientOrderID] = o.Status
	}
	if statuses[b.placed[0].ClientOrderID] != string(domain.OrderStatusUnsent) ||
		statuses[b.placed[1].ClientOrderID] != string(domain.OrderStatusSubmitted) {
		t.Errorf("台帳: %v", statuses)
	}
}

func TestUnconfirmedIsAttributedWhenBrokerHasOrder(t *testing.T) {
	env, _ := todayEnv(t)
	b := &stubBroker{balance: richBalance()}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		// 届いたが応答が返らなかった: 一覧には載っている
		b.history = append(b.history, brokerOrderOf("77/x", req.Symbol, req.Side, 100, req.Trade, domain.OrderStatusSubmitted))
		return nil, errors.New("timeout")
	}
	orders, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || orders != 1 || len(failures) != 0 || len(b.placed) != 1 {
		t.Fatalf("orders=%d failures=%v placed=%d err=%v", orders, failures, len(b.placed), err)
	}
	o := statusOf(t, env, "7203")
	if o.Status != string(domain.OrderStatusSubmitted) || o.BrokerOrderID == nil || *o.BrokerOrderID != "77/x" {
		t.Errorf("帰属されていない: %+v", o)
	}
}

func TestUnconfirmedStaysPendingWhenHistoryUnavailable(t *testing.T) {
	env, _ := todayEnv(t)
	b := &stubBroker{balance: richBalance(), historyErr: errors.New("down")}
	b.place = func(domain.OrderRequest) (*domain.OrderAck, error) { return nil, errors.New("timeout") }
	_, failures, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if len(failures) != 1 || len(b.placed) != 1 {
		t.Fatalf("判定できないのに送り直した: failures=%v placed=%d", failures, len(b.placed))
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusPending) {
		t.Errorf("PENDING のまま次の実行に渡す: %s", o.Status)
	}
	// 次の実行の冒頭: 一覧が取れれば判定できる
	b.historyErr = nil
	summary, err := ResolvePending(env, b, 0)
	if err != nil || summary.NotSent != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusUnsent) {
		t.Errorf("UNSENT になっていない: %s", o.Status)
	}
	// 「発注済み」に数えないので、もう一度 open が走れば送り直す
	b.place = nil
	orders, _, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if orders != 1 || len(b.placed) != 2 {
		t.Errorf("UNSENT の後に送り直されていない: orders=%d placed=%d", orders, len(b.placed))
	}
}

func TestResolvePendingAmbiguousAlertsAndKeepsPending(t *testing.T) {
	env, rep := todayEnv(t)
	b := &stubBroker{balance: richBalance()}
	b.place = func(domain.OrderRequest) (*domain.OrderAck, error) { return nil, errors.New("timeout") }
	// 同じ銘柄・売買で数量の違う未帰属の注文がある → 決められない
	b.history = []domain.Order{brokerOrderOf("9/x", "7203", domain.SideBuy, 300, domain.TradeTypeCash, domain.OrderStatusFilled)}
	PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusPending) {
		t.Errorf("決められないときは PENDING のまま: %s", o.Status)
	}
	if len(rep.alerts) == 0 || len(rep.errors) == 0 {
		t.Errorf("決められないことを知らせていない: alerts=%v errors=%v", rep.alerts, rep.errors)
	}
	if len(b.placed) != 1 {
		t.Errorf("決められないのに送り直した: %d", len(b.placed))
	}
}
