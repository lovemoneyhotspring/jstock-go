package execute

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// recordShortToday は今日の売建（送信済み・台帳で未確定）を台帳に残す。
func recordShortToday(t *testing.T, env Env, symbol string, qty int64) string {
	t.Helper()
	req, err := domain.NewOrderRequest("s-"+symbol, symbol, domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeMarginOpen)
	if err != nil {
		t.Fatal(err)
	}
	id := "B/" + req.ClientOrderID
	price := decimal.NewFromInt(761)
	if err := env.Ledger.Record(req, env.Day, string(domain.OrderStatusSubmitted), &price, &id); err != nil {
		t.Fatal(err)
	}
	return req.ClientOrderID
}

// orderBook は取消を受けると finalStatus / finalFilled に変わる注文の模型。
// 取消の前は SUBMITTED（約定 before 株）を返す。
type orderBook struct {
	before, finalFilled int64
	finalStatus         domain.OrderStatus
	cancelled           bool
	qty                 int64
}

func (ob *orderBook) broker() *stubBroker {
	return &stubBroker{
		balance: richBalance(),
		getOrder: func(id string) (*domain.Order, error) {
			price := decimal.NewFromInt(791)
			if !ob.cancelled {
				status := domain.OrderStatusSubmitted
				if ob.before > 0 {
					status = domain.OrderStatusPartiallyFilled
				}
				return &domain.Order{ClientOrderID: id, Status: status, Quantity: decimal.NewFromInt(ob.qty),
					FilledQuantity: decimal.NewFromInt(ob.before), AvgFillPrice: &price}, nil
			}
			return &domain.Order{ClientOrderID: id, Status: ob.finalStatus, Quantity: decimal.NewFromInt(ob.qty),
				FilledQuantity: decimal.NewFromInt(ob.finalFilled), AvgFillPrice: &price}, nil
		},
		cancel: func(string) error { ob.cancelled = true; return nil },
	}
}

var tobMarks = map[string]string{"8848": "tob_target"}

// 未約定の売建は取り消すだけ。返済は出さない。取り消した銘柄は「建てていない」に戻る。
func TestGuardCancelsUnfilledShort(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	ob := &orderBook{qty: 1300, finalStatus: domain.OrderStatusCancelled}
	b := ob.broker()

	actions, err := GuardCorpEvents(env, b, tobMarks)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || !actions[0].Cancelled || actions[0].Err != nil || actions[0].Returned.IsPositive() {
		t.Fatalf("actions = %+v", actions)
	}
	if len(b.cancelled) != 1 || b.cancelled[0] != id || len(b.placed) != 0 {
		t.Errorf("cancelled=%v placed=%v, want 取消 1 件・発注なし", b.cancelled, b.placed)
	}
	o, _, _ := env.Ledger.Get(id)
	if o.Status != string(domain.OrderStatusCancelled) || !o.IsDead() {
		t.Errorf("台帳 = %s / filled %s, want 約定なしの取消", o.Status, o.FilledQuantity)
	}
	// 2 回目（次の cron）は何もしない
	if actions, _ := GuardCorpEvents(env, b, tobMarks); len(actions) != 0 || len(b.cancelled) != 1 {
		t.Errorf("2 回目: actions=%+v cancelled=%v", actions, b.cancelled)
	}
}

// 一部約定の売建は残りを取り消し、取消が終わってから**最終の約定数量**を返済する。
// 取消中に約定が増えた分も返す。引けの手仕舞い・持ち越しの判定・再実行が重ねない。
func TestGuardCancelsRestAndReturnsPartialFill(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	ob := &orderBook{qty: 1300, before: 300, finalFilled: 500, finalStatus: domain.OrderStatusCancelled}
	b := ob.broker()

	actions, err := GuardCorpEvents(env, b, tobMarks)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Err != nil || !actions[0].Cancelled || !actions[0].Returned.Equal(decimal.NewFromInt(500)) {
		t.Fatalf("actions = %+v", actions)
	}
	if len(b.placed) != 1 {
		t.Fatalf("placed = %+v, want 返済 1 件", b.placed)
	}
	exit := b.placed[0]
	if exit.Side != domain.SideBuy || exit.Trade != domain.TradeTypeMarginClose || !exit.Quantity.Equal(decimal.NewFromInt(500)) {
		t.Errorf("返済 = %s %s %s, want BUY MARGIN_CLOSE 500", exit.Side, exit.Trade, exit.Quantity)
	}
	entry, _, _ := env.Ledger.Get(id)
	if entry.IsDead() || !entry.FilledQuantity.Equal(decimal.NewFromInt(500)) {
		t.Errorf("台帳 = %s / filled %s, want 約定 500 株の取消（死んでいない）", entry.Status, entry.FilledQuantity)
	}

	// 次の cron の guard は重ねない
	if _, err := GuardCorpEvents(env, b, tobMarks); err != nil || len(b.placed) != 1 || len(b.cancelled) != 1 {
		t.Errorf("2 回目: placed=%d cancelled=%d err=%v", len(b.placed), len(b.cancelled), err)
	}
	// 引けの手仕舞いは返済が生きているので重ねない
	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 0 || len(unconfirmed) != 0 {
		t.Errorf("close: targets=%+v unconfirmed=%v err=%v, want 手仕舞いなし", targets, unconfirmed, err)
	}
	// open の再実行はこの銘柄を建てたと数える（枠を埋め直さない）
	placed, _ := PlacedToday(env)
	if placed.Short != 1 || placed.Symbols["8848"] != domain.SideSell {
		t.Errorf("PlacedToday = %+v, want ショート 1（8848）", placed)
	}
	// 台帳が知っている売建は約定した 500 株（台帳外の建玉として掃除しない）
	recorded, _ := recordedByLeg(env, nil)
	if got := recorded[broker.LegOf("8848", domain.TradeTypeMarginOpen, true)]; !got.Equal(decimal.NewFromInt(500)) {
		t.Errorf("recordedByLeg = %s, want 500", got)
	}
}

// 全部約定した売建は取消を送らず、返済だけ出す。
func TestGuardReturnsFilledShort(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	p := decimal.NewFromInt(791)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(1300), &p, nil); err != nil {
		t.Fatal(err)
	}
	b := &stubBroker{balance: richBalance()}
	actions, err := GuardCorpEvents(env, b, tobMarks)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Cancelled || !actions[0].Returned.Equal(decimal.NewFromInt(1300)) {
		t.Fatalf("actions = %+v", actions)
	}
	if len(b.cancelled) != 0 || len(b.placed) != 1 {
		t.Errorf("cancelled=%v placed=%d, want 取消なし・返済 1", b.cancelled, len(b.placed))
	}
}

// 取消が終わったと確かめられなければ返済を出さない（約定数量が決まらない）。
func TestGuardDoesNotReturnWhileCancelPending(t *testing.T) {
	env, _ := newEnv(t)
	recordShortToday(t, env, "8848", 1300)
	// 取消を受けても「取消中」のまま（SUBMITTED、約定 300）
	ob := &orderBook{qty: 1300, before: 300, finalFilled: 300, finalStatus: domain.OrderStatusSubmitted}
	b := ob.broker()
	actions, _ := GuardCorpEvents(env, b, tobMarks)
	if len(actions) != 1 || actions[0].Err == nil || len(b.placed) != 0 {
		t.Errorf("actions=%+v placed=%d, want エラー・返済なし", actions, len(b.placed))
	}
}

// 照会できない注文には取消も返済も出さない。印の無い銘柄・ロングには触らない。
func TestGuardLeavesUnknownAndUnmarked(t *testing.T) {
	env, _ := newEnv(t)
	recordShortToday(t, env, "8848", 1300)
	recordShortToday(t, env, "7203", 100)
	b := &stubBroker{balance: richBalance()} // getOrder が nil を返す
	actions, _ := GuardCorpEvents(env, b, tobMarks)
	if len(actions) != 1 || actions[0].Symbol != "8848" || actions[0].Err == nil {
		t.Errorf("actions = %+v, want 8848 だけ・照会できずエラー", actions)
	}
	if len(b.cancelled) != 0 || len(b.placed) != 0 {
		t.Errorf("cancelled=%v placed=%d, want 何も送らない", b.cancelled, len(b.placed))
	}
	// dry-run は台帳もブローカーも触らない
	if actions, _ := GuardCorpEvents(env, nil, tobMarks); len(actions) != 1 || actions[0].Acted() {
		t.Errorf("dry-run: %+v", actions)
	}
}

// 返済の一部だけ約定して失効したら、引けの手仕舞いは残りだけを出す（全量を出し直さない）。
func TestRefreshEntriesExitsOnlyRemainder(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	p := decimal.NewFromInt(791)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(1300), &p, nil); err != nil {
		t.Fatal(err)
	}
	exitID := recordExit(t, env, env.Day, "8848", domain.SideBuy, 1300, domain.OrderStatusSubmitted)
	if err := env.Ledger.UpdateStatus(exitID, domain.OrderStatusExpired, decimal.NewFromInt(1000), &p, nil); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, &stubBroker{}, entries)
	if err != nil || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(300)) {
		t.Errorf("targets = %+v err=%v, want 残り 300 株", targets, err)
	}
}
