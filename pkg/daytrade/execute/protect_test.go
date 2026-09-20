package execute

import (
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// protectBroker は建て注文（entryID）が filled 株まで約定していて、保険の注文は取消を受けると
// 終わる（cancelWorks が偽なら生きたまま）模型。
type protectBroker struct {
	*stubBroker
	entryID      string
	entryQty     int64
	filled       int64
	cancelWorks  bool
	protectState map[string]domain.OrderStatus
}

func newProtectBroker(entryID string, qty, filled int64) *protectBroker {
	pb := &protectBroker{stubBroker: &stubBroker{balance: richBalance()}, entryID: entryID,
		entryQty: qty, filled: filled, cancelWorks: true, protectState: map[string]domain.OrderStatus{}}
	p := decimal.NewFromInt(761)
	pb.stubBroker.getOrder = func(id string) (*domain.Order, error) {
		if id == pb.entryID {
			status := domain.OrderStatusFilled
			if pb.filled < pb.entryQty {
				status = domain.OrderStatusPartiallyFilled
			}
			return &domain.Order{ClientOrderID: id, Status: status, Quantity: decimal.NewFromInt(pb.entryQty),
				FilledQuantity: decimal.NewFromInt(pb.filled), AvgFillPrice: &p}, nil
		}
		st, ok := pb.protectState[id]
		if !ok {
			st = domain.OrderStatusSubmitted
		}
		return &domain.Order{ClientOrderID: id, Status: st, Quantity: decimal.NewFromInt(1)}, nil
	}
	pb.stubBroker.cancel = func(id string) error {
		if pb.cancelWorks {
			pb.protectState[id] = domain.OrderStatusCancelled
		}
		return nil
	}
	return pb
}

func fill(t *testing.T, env Env, id string, qty int64) {
	t.Helper()
	p := decimal.NewFromInt(761)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(qty), &p, nil); err != nil {
		t.Fatal(err)
	}
}

// 約定したぶんだけ、引けの返済を置く。約定が無ければ置かない。二重に置かない。
func TestProtectEntriesPlacesClosingExitOnce(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 400)

	actions, err := ProtectEntries(env, pb)
	if err != nil || len(actions) != 1 || actions[0].Err != nil {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
	if len(pb.placed) != 1 {
		t.Fatalf("placed = %d, want 1", len(pb.placed))
	}
	req := pb.placed[0]
	if req.Condition != domain.ConditionClosing || req.Side != domain.SideSell ||
		req.Trade != domain.TradeTypeMarginClose || !req.Quantity.Equal(decimal.NewFromInt(400)) {
		t.Errorf("保険の注文 = %+v, want 引け・返済売り・400 株", req)
	}
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 1 {
		t.Errorf("2 回目: placed=%d err=%v, want 重ねない", len(pb.placed), err)
	}

	// 約定 0 の建て注文には置かない
	env2, _ := newEnv(t)
	id2 := recordLongToday(t, env2, "6758", 300)
	pb2 := newProtectBroker(id2, 300, 0)
	pb2.filled = 0
	pb2.stubBroker.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusSubmitted, Quantity: decimal.NewFromInt(300)}, nil
	}
	if _, err := ProtectEntries(env2, pb2); err != nil || len(pb2.placed) != 0 {
		t.Errorf("約定なし: placed=%d err=%v", len(pb2.placed), err)
	}
}

// 約定が後から増えたら、増えたぶんだけ別の注文で置く（同じ株数でも ID が衝突しない）。
func TestProtectEntriesTopsUpGrowth(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 200)
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 1 {
		t.Fatalf("1 回目: placed=%d err=%v", len(pb.placed), err)
	}
	pb.filled = 400
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 2 {
		t.Fatalf("2 回目: placed=%d err=%v", len(pb.placed), err)
	}
	if !pb.placed[1].Quantity.Equal(decimal.NewFromInt(200)) || pb.placed[0].ClientOrderID == pb.placed[1].ClientOrderID {
		t.Errorf("増えたぶん = %+v / %+v, want 200 株を別の ID で", pb.placed[0], pb.placed[1])
	}
}

// 拒否された日は、それ以上置かない（毎回同じ理由で拒否され、通知が増えるだけ）。
func TestProtectEntriesStopsAfterRejection(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 400)
	pb.stubBroker.place = func(domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, &broker.OrderRejectedError{Message: "執行条件が受け付けられません"}
	}
	actions, err := ProtectEntries(env, pb)
	if err != nil || len(actions) != 1 || actions[0].Err == nil {
		t.Fatalf("1 回目: actions=%+v err=%v, want 失敗 1 件", actions, err)
	}
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 1 {
		t.Errorf("2 回目: placed=%d err=%v, want 送らない", len(pb.placed), err)
	}
}

// 引け: 保険の注文は生きていても「手仕舞い済み」と数えない。取り消してから 15:20 の成行を出す。
// 出す成行は条件なし・保険とは別の ID。
func TestRefreshEntriesReleasesProtectionAndExitsAtMarket(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 400)
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 1 {
		t.Fatalf("保険: placed=%d err=%v", len(pb.placed), err)
	}
	protectID := pb.placed[0].ClientOrderID

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, pb, entries)
	if err != nil || len(unconfirmed) != 0 {
		t.Fatalf("unconfirmed=%v err=%v", unconfirmed, err)
	}
	if len(pb.cancelled) != 1 || pb.cancelled[0] != protectID {
		t.Fatalf("cancelled = %v, want 保険 %s だけ", pb.cancelled, protectID)
	}
	if len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(400)) || targets[0].Protective {
		t.Fatalf("targets = %+v, want 通常の手仕舞い 400 株", targets)
	}
	if failures := PlaceExits(env, pb, targets); len(failures) != 0 {
		t.Fatal(failures)
	}
	if len(pb.placed) != 2 {
		t.Fatalf("placed = %d, want 保険 + 成行", len(pb.placed))
	}
	exit := pb.placed[1]
	if exit.Condition != domain.ConditionNone || exit.ClientOrderID == protectID || !exit.Quantity.Equal(decimal.NewFromInt(400)) {
		t.Errorf("成行の手仕舞い = %+v, want 条件なし・別の ID・400 株", exit)
	}

	// 次の回（15:24）は成行が生きているので重ねない。取消済みの保険を再び数えない
	entries, _, _ = LiveEntries(env)
	targets, _, err = RefreshEntries(env, pb, entries)
	if err != nil || len(targets) != 0 || len(pb.cancelled) != 1 {
		t.Errorf("2 回目: targets=%+v cancelled=%d err=%v, want 何もしない", targets, len(pb.cancelled), err)
	}
}

// 取消の完了を確かめられないときは成行を出さず、保険に任せて知らせる（二重に手仕舞わない）。
func TestRefreshEntriesLeavesProtectionWhenCancelUnconfirmed(t *testing.T) {
	env, rep := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 400)
	if _, err := ProtectEntries(env, pb); err != nil {
		t.Fatal(err)
	}
	pb.cancelWorks = false // 取消を送っても生きたまま

	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, pb, entries)
	if err != nil || len(targets) != 0 || len(unconfirmed) != 0 {
		t.Fatalf("targets=%+v unconfirmed=%v err=%v, want 成行を出さない", targets, unconfirmed, err)
	}
	if !rep.warned("daytrade.protect_held") || len(rep.alerts) != 1 || !strings.Contains(rep.alerts[0], "保険") {
		t.Errorf("警告=%v alerts=%v, want 保険を取り消せない通知", rep.warned("daytrade.protect_held"), rep.alerts)
	}
}

// 保険が引けで約定して終わっていれば「手仕舞い済み」。成行を出さない（二重に手仕舞わない）。
func TestRefreshEntriesCountsFilledProtection(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	pb := newProtectBroker(id, 400, 400)
	if _, err := ProtectEntries(env, pb); err != nil {
		t.Fatal(err)
	}
	fill(t, env, pb.placed[0].ClientOrderID, 400)

	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, pb, entries)
	if err != nil || len(targets) != 0 || len(pb.cancelled) != 0 {
		t.Errorf("targets=%+v cancelled=%v err=%v, want 約定済みの保険で完了", targets, pb.cancelled, err)
	}
}

// TOB の guard は、返済の前にその銘柄の保険を取り消す（返済できる建玉を保険が押さえている）。
func TestGuardReleasesProtectionBeforeReturning(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	p := decimal.NewFromInt(791)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(1300), &p, nil); err != nil {
		t.Fatal(err)
	}
	pb := newProtectBroker(id, 1300, 1300)
	if _, err := ProtectEntries(env, pb); err != nil || len(pb.placed) != 1 {
		t.Fatalf("保険: placed=%d err=%v", len(pb.placed), err)
	}
	if pending, _ := GuardPending(env, tobMarks); len(pending) != 1 {
		t.Errorf("GuardPending = %v, want 保険だけでは返済待ちのまま", pending)
	}

	actions, err := GuardCorpEvents(env, pb, tobMarks)
	if err != nil || len(actions) != 1 || actions[0].Err != nil || !actions[0].Returned.Equal(decimal.NewFromInt(1300)) {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
	if len(pb.cancelled) != 1 || len(pb.placed) != 2 {
		t.Fatalf("cancelled=%v placed=%d, want 保険を取り消してから返済 1 件", pb.cancelled, len(pb.placed))
	}
	if pb.placed[1].Condition != domain.ConditionNone || pb.placed[1].Side != domain.SideBuy {
		t.Errorf("返済 = %+v, want 条件なしの返済買い", pb.placed[1])
	}
}

// 保険を取り消せないときの guard は返済を出さず、次の回に回す。
func TestGuardDoesNotReturnWhileProtectionCannotBeCancelled(t *testing.T) {
	env, _ := newEnv(t)
	id := recordShortToday(t, env, "8848", 1300)
	p := decimal.NewFromInt(791)
	if err := env.Ledger.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(1300), &p, nil); err != nil {
		t.Fatal(err)
	}
	pb := newProtectBroker(id, 1300, 1300)
	if _, err := ProtectEntries(env, pb); err != nil {
		t.Fatal(err)
	}
	pb.cancelWorks = false
	actions, _ := GuardCorpEvents(env, pb, tobMarks)
	if len(actions) != 1 || actions[0].Err == nil || len(pb.placed) != 1 {
		t.Errorf("actions=%+v placed=%d, want エラーで返済を出さない", actions, len(pb.placed))
	}
}

func TestExitRequestProtectiveCondition(t *testing.T) {
	env, _ := newEnv(t)
	id := recordLongToday(t, env, "7203", 400)
	entry, _, _ := env.Ledger.Get(id)
	normal, _ := ExitRequestAs(ExitTarget{Entry: entry, Quantity: decimal.NewFromInt(400)}, env.Day, env.Cfg, 0, "x")
	prot, _ := ExitRequestAs(ExitTarget{Entry: entry, Quantity: decimal.NewFromInt(400), Protective: true}, env.Day, env.Cfg, 0, "x")
	if normal.Condition != domain.ConditionNone || prot.Condition != domain.ConditionClosing {
		t.Errorf("条件: normal=%q protective=%q", normal.Condition, prot.Condition)
	}
	if normal.ClientOrderID == prot.ClientOrderID {
		t.Error("保険と通常の手仕舞いが同じ ID（種を分けていない）")
	}
}
