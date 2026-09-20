package execute

import (
	"errors"
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
	cancelErr           error // 取消の応答（状態は変わる。取消の間に約定し終えた場合を作る）
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
		cancel: func(string) error { ob.cancelled = true; return ob.cancelErr },
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

// 取消がエラーで、実は全部約定していたら「取り消した」とは言わず、返済だけ出す。
func TestGuardCancelErrorButFilled(t *testing.T) {
	env, _ := newEnv(t)
	recordShortToday(t, env, "8848", 1300)
	ob := &orderBook{qty: 1300, finalFilled: 1300, finalStatus: domain.OrderStatusFilled,
		cancelErr: errors.New("約定済みのため取消できません")}
	b := ob.broker()
	actions, err := GuardCorpEvents(env, b, tobMarks)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Cancelled || actions[0].Err != nil || !actions[0].Returned.Equal(decimal.NewFromInt(1300)) {
		t.Fatalf("actions = %+v, want 取消なし・1300 株の返済", actions)
	}
}

// FILLED なのに約定数量の入っていない行は注文数量が建っているとみなす（返済する・台帳外と読まない）。
func TestGuardFilledWithoutQuantity(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.Zero, nil, nil); err != nil {
		t.Fatal(err)
	}
	recorded, _ := recordedByLeg(env, nil)
	if got := recorded[broker.LegOf("8848", domain.TradeTypeMarginOpen, true)]; !got.Equal(decimal.NewFromInt(1300)) {
		t.Errorf("recordedByLeg = %s, want 1300", got)
	}
	b := &stubBroker{balance: richBalance()}
	actions, _ := GuardCorpEvents(env, b, tobMarks)
	if len(actions) != 1 || !actions[0].Returned.Equal(decimal.NewFromInt(1300)) || len(b.placed) != 1 {
		t.Errorf("actions=%+v placed=%d, want 1300 株の返済", actions, len(b.placed))
	}
}

// 処置の残りは台帳だけで判定する。済んだら空（guard は接続しない）。
func TestGuardPending(t *testing.T) {
	env, _ := newEnv(t)
	recordShortToday(t, env, "8848", 1300)
	recordShortToday(t, env, "7203", 100)
	pending, err := GuardPending(env, tobMarks)
	if err != nil || len(pending) != 1 || pending[0] != "8848" {
		t.Fatalf("処置の前: pending=%v err=%v, want [8848]", pending, err)
	}
	ob := &orderBook{qty: 1300, before: 300, finalFilled: 500, finalStatus: domain.OrderStatusCancelled}
	if _, err := GuardCorpEvents(env, ob.broker(), tobMarks); err != nil {
		t.Fatal(err)
	}
	if pending, _ := GuardPending(env, tobMarks); len(pending) != 0 {
		t.Errorf("処置の後: pending=%v, want なし（返済が生きている）", pending)
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

// recordLongToday は今日の買い建て（送信済み・台帳で未確定）を台帳に残す。
func recordLongToday(t *testing.T, env Env, symbol string, qty int64) string {
	t.Helper()
	req, err := domain.NewOrderRequest("l-"+symbol, symbol, domain.SideBuy, domain.OrderTypeMarket,
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

// filledExit は台帳の手仕舞い注文が注文数量どおり全部約定した、という照会の応答。
func filledExit(env Env, id string, price *decimal.Decimal) *domain.Order {
	o, _, _ := env.Ledger.Get(id)
	return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
		Quantity: o.Quantity, FilledQuantity: o.Quantity, AvgFillPrice: price}
}

// 引け: 寄らないまま板に残っている買いは取り消す。約定が無ければ手仕舞いは出さない。
func TestRefreshEntriesCancelsUnfilledEntry(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	ob := &orderBook{qty: 400, finalStatus: domain.OrderStatusCancelled}
	b := ob.broker()

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 0 || len(unconfirmed) != 0 {
		t.Fatalf("targets=%+v unconfirmed=%v err=%v, want 手仕舞いなし", targets, unconfirmed, err)
	}
	if len(b.cancelled) != 1 || b.cancelled[0] != id {
		t.Fatalf("cancelled = %v, want %s", b.cancelled, id)
	}
	if entry, _, _ := env.Ledger.Get(id); entry.Status != string(domain.OrderStatusCancelled) {
		t.Errorf("台帳 = %s, want CANCELLED", entry.Status)
	}
	// 次の回（15:24）は確定済みなので聞かない・取り消さない
	entries, _, _ = LiveEntries(env)
	if _, _, err := RefreshEntries(env, b, entries); err != nil || len(b.cancelled) != 1 {
		t.Errorf("2 回目: cancelled=%d err=%v", len(b.cancelled), err)
	}
}

// 引け: 一部約定の残りを取り消し、取消の間に増えた分も含めた確定数量で手仕舞う。
func TestRefreshEntriesCancelsRestAndExitsConfirmedFill(t *testing.T) {
	env, _ := newEnv(t)
	recordLongToday(t, env, "7203", 400)
	ob := &orderBook{qty: 400, before: 100, finalFilled: 200, finalStatus: domain.OrderStatusCancelled}
	b := ob.broker()

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 0 {
		t.Fatalf("unconfirmed=%v err=%v", unconfirmed, err)
	}
	if len(b.cancelled) != 1 || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("cancelled=%v targets=%+v, want 取消 1 件・手仕舞い 200 株", b.cancelled, targets)
	}
}

// 引け: 全部約定した買いには取消を送らない。
func TestRefreshEntriesDoesNotCancelFilledEntry(t *testing.T) {
	env, _ := newEnv(t)
	recordLongToday(t, env, "7203", 400)
	b := &stubBroker{getOrder: func(id string) (*domain.Order, error) {
		p := decimal.NewFromInt(761)
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled, Quantity: decimal.NewFromInt(400),
			FilledQuantity: decimal.NewFromInt(400), AvgFillPrice: &p}, nil
	}}
	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, b, entries)
	if err != nil || len(b.cancelled) != 0 || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(400)) {
		t.Errorf("cancelled=%v targets=%+v err=%v, want 取消なし・400 株", b.cancelled, targets, err)
	}
}

// 引け: 取消の完了を確かめられないときは、分かっている約定分を手仕舞い、人に知らせる。
// 次の回で約定が増えていたら、増えた分だけを足して手仕舞う。
func TestRefreshEntriesCancelUnconfirmedExitsKnownFillThenGrowth(t *testing.T) {
	env, rep := newEnv(t)
	entryID := recordLongToday(t, env, "7203", 400)
	status, filled := domain.OrderStatusPartiallyFilled, int64(100)
	b := &stubBroker{balance: richBalance(), getOrder: func(id string) (*domain.Order, error) {
		p := decimal.NewFromInt(761)
		if id != entryID {
			// 前の回の手仕舞い（次の回が聞き直す）は全部約定している
			return filledExit(env, id, &p), nil
		}
		return &domain.Order{ClientOrderID: id, Status: status, Quantity: decimal.NewFromInt(400),
			FilledQuantity: decimal.NewFromInt(filled), AvgFillPrice: &p}, nil
	}}

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 1 || !rep.warned("daytrade.entry_cancel") {
		t.Fatalf("unconfirmed=%v err=%v, want 取消未確認 1 件と警告", unconfirmed, err)
	}
	if len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("targets=%+v, want 100 株", targets)
	}
	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatal(failures)
	}

	// 次の回: 取消が通り、その間に 300 株まで約定していた。増えた 200 株だけを手仕舞う
	status, filled = domain.OrderStatusCancelled, 300
	entries, _, _ = LiveEntries(env)
	targets, unconfirmed, err = RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 0 || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(200)) {
		t.Errorf("2 回目: targets=%+v unconfirmed=%v err=%v, want 残り 200 株", targets, unconfirmed, err)
	}
}

// 引け: 手仕舞いを出した後に**同じ株数**だけ約定が増えたとき、2 回目の手仕舞いが 1 回目と同じ
// client_order_id になって「発注済み（冪等）」で飛ばされてはいけない（黙って持ち越す）。
func TestRefreshEntriesGrowthOfSameQuantityIsNotSwallowedAsIdempotent(t *testing.T) {
	env, _ := newEnv(t)
	entryID := recordLongToday(t, env, "7203", 400)
	status, filled := domain.OrderStatusPartiallyFilled, int64(200)
	b := &stubBroker{balance: richBalance(), getOrder: func(id string) (*domain.Order, error) {
		p := decimal.NewFromInt(761)
		if id != entryID {
			// 前の回の手仕舞い（次の回が聞き直す）は全部約定している
			return filledExit(env, id, &p), nil
		}
		return &domain.Order{ClientOrderID: id, Status: status, Quantity: decimal.NewFromInt(400),
			FilledQuantity: decimal.NewFromInt(filled), AvgFillPrice: &p}, nil
	}}

	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("1 回目: targets=%+v err=%v, want 200 株", targets, err)
	}
	if failures := PlaceExits(env, b, targets); len(failures) != 0 || len(b.placed) != 1 {
		t.Fatalf("1 回目: failures=%v placed=%d", failures, len(b.placed))
	}

	// 取消が通る前に残りの 200 株も約定していた
	status, filled = domain.OrderStatusCancelled, 400
	entries, _, _ = LiveEntries(env)
	targets, _, err = RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(200)) {
		t.Fatalf("2 回目: targets=%+v err=%v, want 増えた 200 株", targets, err)
	}
	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatal(failures)
	}
	if len(b.placed) != 2 || b.placed[0].ClientOrderID == b.placed[1].ClientOrderID {
		t.Fatalf("placed = %+v, want 別の ID で 2 件（同じ ID だと 2 回目が送られない）", b.placed)
	}

	// 同じ状態での再実行は重ねない（冪等）
	entries, _, _ = LiveEntries(env)
	targets, _, _ = RefreshEntries(env, b, entries)
	if len(targets) != 0 {
		t.Errorf("3 回目: targets=%+v, want 手仕舞いなし", targets)
	}
}

// 引け: 台帳に一部約定が残っている未確定の建て注文を照会できないとき、約定分は手仕舞うが、
// 残りが板に生きているかもしれないので黙って正常終了しない。
func TestRefreshEntriesUnqueryablePartialIsReported(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	p := decimal.NewFromInt(761)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusPartiallyFilled, decimal.NewFromInt(100), &p, nil); err != nil {
		t.Fatal(err)
	}
	b := &stubBroker{} // 照会は空

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("targets=%+v err=%v, want 台帳の約定 100 株", targets, err)
	}
	if len(unconfirmed) != 1 || len(b.cancelled) != 0 {
		t.Errorf("unconfirmed=%v cancelled=%v, want 知らせる 1 件・取消は送らない", unconfirmed, b.cancelled)
	}
}

// 引け: FILLED なのに約定数量が入っていない台帳行は注文数量で手仕舞う（0 と読んで持ち越さない）。
func TestRefreshEntriesFilledWithoutQuantity(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.Zero, nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, &stubBroker{}, entries)
	if err != nil || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(400)) {
		t.Errorf("targets=%+v err=%v, want 400 株", targets, err)
	}
}
