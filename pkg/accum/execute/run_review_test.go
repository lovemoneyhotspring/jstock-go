package execute

import (
	"errors"
	"strings"
	"testing"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/shopspring/decimal"
)

// 2026-09-24 のレビュー（売買単位・拒否の記録・保留の通知）の再現。

// reviewEnv は 1306 を予算 20 万で積み立てる run の土台（RunAccumulation は実時計で動く）。
type reviewEnv struct {
	cfg       *accumcfg.AccumConfig
	store     *data.BarStore
	led       *ledger.Ledger
	thisMonth string
}

func newReviewEnv(t *testing.T) reviewEnv {
	t.Helper()
	jst := clock.ToZone(clock.NowUTC(), clock.Tokyo)
	if jst.Day() == 1 {
		t.Skip("月の初日は今月の確定足が無く、発注の経路を通らない")
	}
	monthStart := time.Date(jst.Year(), jst.Month(), 1, 0, 0, 0, 0, time.UTC)
	yesterday := time.Date(jst.Year(), jst.Month(), jst.Day()-1, 0, 0, 0, 0, time.UTC)
	store := data.NewBarStore(t.TempDir())
	writeBars(t, store, "1306.T", monthStart.AddDate(0, 0, -3).Format("2006-01-02"),
		yesterday.Format("2006-01-02"), 1000)
	led := newLedger(t)
	if err := led.MarkStarted("1306", "2025-01-01"); err != nil {
		t.Fatal(err)
	}
	return reviewEnv{cfg: planConfig(200_000, window.Unrestricted()), store: store, led: led,
		thisMonth: monthStart.Format("2006-01-02")}
}

func (e reviewEnv) run(b broker.Broker, live bool) error {
	return RunAccumulation(e.cfg, b, e.store, e.led, &logging.Logger{}, nil, live, false)
}

// 1. 上書きもブローカーの値も無い銘柄は、既定の 100 株で丸めて「単元未満で見送り」に
// せず、発注できなかった銘柄（OrdersFailedError → 通知・exit 1）にする。
func TestRunAccumulationFailsWhenLotSizeUnknown(t *testing.T) {
	e := newReviewEnv(t)
	e.cfg.Execution.LotSizeOverrides = nil
	stubAlerts(t)
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000)} // 銘柄マスタに無い（取れなかった）

	err := e.run(b, true)
	var failed *OrdersFailedError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "売買単位が分からない") {
		t.Fatalf("OrdersFailedError（売買単位が分からない）を返すべき: %v", err)
	}
	if len(b.placed) != 0 || b.previews != 0 {
		t.Errorf("売買単位が分からないのに見積り・発注した: %d / %d", b.previews, len(b.placed))
	}
}

// 2. dry-run（PaperBroker）は与えていない銘柄の売買単位を持たない。以前は 100 株を
// 返し、本発注（銘柄マスタの値）と違う計画になっていた。100 株では丸めないが、失敗にすると
// lot_size_overrides に無い銘柄（1629・2559）で dry-run が毎回、失敗の通知と非 0 終了に
// なるので、dry-run に限り見送り（通知しない・エラーを返さない）にする。本発注は失敗のまま（1.）。
func TestRunAccumulationDryRunSkipsWhenPaperHasNoLotSize(t *testing.T) {
	e := newReviewEnv(t)
	e.cfg.Execution.LotSizeOverrides = nil
	alerts := stubAlerts(t)

	if err := e.run(broker.NewPaperBroker(decimal.Zero, "open"), false); err != nil {
		t.Fatalf("dry-run の売買単位の不明を失敗にした: %v", err)
	}
	if len(*alerts) != 0 {
		t.Errorf("dry-run の売買単位の不明で通知した: %v", *alerts)
	}
	if recent, err := e.led.Recent(1); err != nil || len(recent) != 0 {
		t.Errorf("株数が決まらないのに dry_run を記録した: %+v (err: %v)", recent, err)
	}

	// 計画の段では見送りの理由が dry-run 用の文になる
	planned, _, err := PlanOrders(e.cfg, e.store, e.led, clock.NowUTC(), false, false, nil)
	if err != nil || len(planned) != 1 || !planned[0].LotUnknown || !planned[0].Failed {
		t.Fatalf("PlanOrders は売買単位の不明を LotUnknown の失敗として返すべき: %+v (err: %v)", planned, err)
	}

	// 上書きがあれば dry-run でも計画どおり記録する
	e2 := newReviewEnv(t)
	if err := e2.run(broker.NewPaperBroker(decimal.Zero, "open"), false); err != nil {
		t.Fatalf("上書きのある銘柄の dry-run が失敗した: %v", err)
	}
	recent, err := e2.led.Recent(1)
	if err != nil || len(recent) != 1 || recent[0].Status != ledger.DryRunStatus {
		t.Errorf("dry_run の記録 = %+v (err: %v)", recent, err)
	}
}

// 2 の 2. dry-run の回の通知は件名に [dry-run] を付け、本発注の失敗と見分けられるようにする。
func TestRunAccumulationDryRunAlertsAreMarked(t *testing.T) {
	e := newReviewEnv(t)
	alerts := stubAlerts(t)
	// 足の無い銘柄（失敗）と、前日以前の送信結果不明（保留の通知）を作る
	e.cfg.Tactics[0].Symbols = append(e.cfg.Tactics[0].Symbols, "2559.T")
	e.cfg.Execution.LotSizeOverrides["2559.T"] = 1
	recordOrder(t, e.led, "前日の不明", string(domain.OrderStatusPending), &e.thisMonth, 200_000)
	backdate(t, e.led, "前日の不明")

	err := e.run(broker.NewPaperBroker(decimal.Zero, "open"), false)
	var failed *OrdersFailedError
	if !errors.As(err, &failed) || !failed.DryRun || !strings.Contains(err.Error(), "2559") {
		t.Fatalf("dry-run の印つきの OrdersFailedError を返すべき: %v (%+v)", err, failed)
	}
	if len(*alerts) != 1 || !strings.HasPrefix((*alerts)[0], DryRunTitlePrefix) {
		t.Errorf("保留の通知の件名に [dry-run] が無い: %v", *alerts)
	}

	// 本発注の回は印を付けない
	*alerts = nil
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000),
		lots: map[string]decimal.Decimal{"1306": dec(100), "2559": dec(1)}}
	err = e.run(b, true)
	if !errors.As(err, &failed) || failed.DryRun {
		t.Fatalf("本発注の OrdersFailedError に dry-run の印が付いた: %v (%+v)", err, failed)
	}
	for _, a := range *alerts {
		if strings.HasPrefix(a, DryRunTitlePrefix) {
			t.Errorf("本発注の通知に [dry-run] が付いた: %s", a)
		}
	}
}

// 3. 拒否されたあと台帳を REJECTED に書けなければ、「発注拒否」として次へ進まず止める。
// 以前は拒否を %w で包んでいたので errors.As(OrderRejectedError) が真になり、次の銘柄へ進んでいた。
func TestRunAccumulationStopsWhenRejectionNotRecorded(t *testing.T) {
	e := newReviewEnv(t)
	stubAlerts(t)
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000),
		placeErr: &broker.OrderRejectedError{Message: "値幅制限"}}
	b.onPlace = func() { _ = e.led.Close() }

	err := e.run(b, true)
	if err == nil || !strings.Contains(err.Error(), "台帳を REJECTED にできません") {
		t.Fatalf("台帳を書けないことを返すべき: %v", err)
	}
	var failed *OrdersFailedError
	if errors.As(err, &failed) {
		t.Errorf("「発注できなかった銘柄」として次へ進む扱いになっている: %v", err)
	}
	var notRecorded *ErrRejectionNotRecorded
	if !errors.As(err, &notRecorded) {
		t.Errorf("ErrRejectionNotRecorded を返すべき: %v", err)
	}
	var rejected *broker.OrderRejectedError
	if errors.As(err, &rejected) {
		t.Error("拒否（次へ進む）として読めてしまう")
	}
}

// 3 の 2. placeRecorded 単体でも同じ（拒否はしたが台帳を REJECTED にできない）。
func TestPlaceRecordedRejectionNotRecordedIsNotRejected(t *testing.T) {
	led := newLedger(t)
	req := newRequest(t, "注文5", "1306", 100)
	b := &runBroker{placeErr: &broker.OrderRejectedError{Message: "値幅制限"}}
	b.onPlace = func() { _ = led.Close() }

	_, err := placeRecorded(b, led, req, "2026-09-01", dec(100000), domain.MarketJP)
	var rejected *broker.OrderRejectedError
	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("拒否として読めてはいけない: %v", err)
	}
	if !strings.Contains(err.Error(), "値幅制限") {
		t.Errorf("拒否の理由を併記する: %v", err)
	}
}

// 4. 設定の上書きとブローカーの値が両方あって違えば、どちらかで丸めず失敗にする（両方の値を出す）。
func TestRunAccumulationFailsWhenLotSizesDisagree(t *testing.T) {
	e := newReviewEnv(t)
	e.cfg.Execution.LotSizeOverrides = map[string]int{"1306.T": 10}
	stubAlerts(t)
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000),
		lots: map[string]decimal.Decimal{"1306": dec(1)}}

	err := e.run(b, true)
	var failed *OrdersFailedError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "10 株") || !strings.Contains(err.Error(), "（1 株") {
		t.Fatalf("OrdersFailedError（両方の値つき）を返すべき: %v", err)
	}
	if len(b.placed) != 0 {
		t.Errorf("売買単位が食い違うのに発注した: %d 件", len(b.placed))
	}
}

// 5. 銘柄マスタは今日出す注文が立つ run でだけ引く。今月分を出し終えた run では引かない。
func TestRunAccumulationLooksUpLotsOnlyWhenOrdering(t *testing.T) {
	e := newReviewEnv(t)
	stubAlerts(t)
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000),
		lots: map[string]decimal.Decimal{"1306": dec(100)}}
	if err := e.run(b, true); err != nil {
		t.Fatal(err)
	}
	if b.lotLookups != 1 || len(b.placed) != 1 {
		t.Fatalf("注文が立つ run: 銘柄マスタ %d 回・発注 %d 件, want 1・1", b.lotLookups, len(b.placed))
	}
	// 2 回目は今月分を出し終えている
	if err := e.run(b, true); err != nil {
		t.Fatal(err)
	}
	if b.lotLookups != 1 {
		t.Errorf("今月分を出し終えた run でも銘柄マスタを引いた: 計 %d 回", b.lotLookups)
	}
}

// backdate は台帳の行の送った時刻を前日にする（前日以前の PENDING を作る）。
func backdate(t *testing.T, led *ledger.Ledger, clientOrderID string) {
	t.Helper()
	db, err := storage.OpenSQLite(led.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	yesterday := clock.NowUTC().Add(-36 * time.Hour).Format(time.RFC3339)
	if _, err := db.Exec("UPDATE orders SET placed_at = ? WHERE client_order_id = ?;", yesterday, clientOrderID); err != nil {
		t.Fatal(err)
	}
}

// 6. 前日以前の PENDING は次の run でも判定しない。件名は「照会できません」ではなく
// `accum pending resolve` が要ることを言う。
func TestRunAccumulationAlertsEarlierPendingNeedsResolve(t *testing.T) {
	e := newReviewEnv(t)
	alerts := stubAlerts(t)
	// 今月分（20 万）に数わる PENDING。今日の注文は立たない
	recordOrder(t, e.led, "前日の不明", string(domain.OrderStatusPending), &e.thisMonth, 200_000)
	backdate(t, e.led, "前日の不明")
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000)}

	if err := e.run(b, true); err != nil {
		t.Fatalf("今日の注文が立たない回はエラーにしない: %v", err)
	}
	if len(*alerts) != 1 {
		t.Fatalf("通知 = %d 通, want 1: %v", len(*alerts), *alerts)
	}
	got := (*alerts)[0]
	if !strings.Contains(got, "accum pending resolve") || strings.Contains(got, "照会できません") {
		t.Errorf("件名が実態（resolve まで残る）に合っていない: %s", got)
	}
}

// 6 の 2. 同じ PENDING の銘柄に今日の注文が立つと、発注できなかった銘柄（OrdersFailedError。
// cmd/accum が 1 通送る）として知らせる。以前はそれとは別に「照会できません」も送り 2 通になった。
func TestRunAccumulationAlertsEarlierPendingOnce(t *testing.T) {
	e := newReviewEnv(t)
	alerts := stubAlerts(t)
	// 額 0 の行にして、差額（20 万）が今日の注文として立つようにする
	recordOrder(t, e.led, "前日の不明", string(domain.OrderStatusPending), &e.thisMonth, 0)
	backdate(t, e.led, "前日の不明")
	b := &runBroker{buyingPower: dec(1_000_000), cost: dec(101_000)}

	err := e.run(b, true)
	var failed *OrdersFailedError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "送信結果不明の注文 前日の不明") {
		t.Fatalf("OrdersFailedError（送信結果不明が残る）を返すべき: %v", err)
	}
	if len(*alerts) != 0 {
		t.Errorf("失敗の通知とは別に保留の通知を送った（2 通になる）: %v", *alerts)
	}
}

// 6 の 2 の 2. 買付余力を照会できない回も同じ。PENDING の銘柄は失敗の行で知らせ、
// 保留の通知を別に送らない（以前はこの分岐だけ印を付けずに返り、2 通になった）。
func TestRunAccumulationAlertsEarlierPendingOnceWhenBalanceFails(t *testing.T) {
	e := newReviewEnv(t)
	alerts := stubAlerts(t)
	recordOrder(t, e.led, "前日の不明", string(domain.OrderStatusPending), &e.thisMonth, 0)
	backdate(t, e.led, "前日の不明")
	b := &runBroker{balanceErr: errors.New("余力照会がタイムアウト"), cost: dec(101_000)}

	err := e.run(b, true)
	var failed *OrdersFailedError
	if !errors.As(err, &failed) || !strings.Contains(err.Error(), "送信結果不明の注文 前日の不明") {
		t.Fatalf("OrdersFailedError（送信結果不明が残る）を返すべき: %v", err)
	}
	if len(*alerts) != 0 {
		t.Errorf("失敗の通知とは別に保留の通知を送った（2 通になる）: %v", *alerts)
	}
	if len(b.placed) != 0 {
		t.Errorf("余力が分からないのに発注した: %d 件", len(b.placed))
	}
}

// 6 の 3. 前日以前と今日の保留が混ざるときのダイジェストの文。前日以前を「次の run で再判定」と書かない。
func TestDescribeHeld(t *testing.T) {
	pending, other := describeHeld([]UnresolvedOrder{
		{ClientOrderID: "a", Status: string(domain.OrderStatusPending), NeedsResolve: true},
		{ClientOrderID: "b", Status: string(domain.OrderStatusPending)},
		{ClientOrderID: "c", Status: string(domain.OrderStatusSubmitted)},
	})
	if !strings.Contains(pending, "2 件") || !strings.Contains(pending, "1 件は前日以前") ||
		!strings.Contains(pending, "accum pending resolve") || !strings.Contains(pending, "1 件は次の run で再判定") {
		t.Errorf("pending = %s", pending)
	}
	if !strings.Contains(other, "1 件の注文を照会できず") {
		t.Errorf("other = %s", other)
	}
	if p, o := describeHeld([]UnresolvedOrder{{Status: string(domain.OrderStatusPending), NeedsResolve: true}}); strings.Contains(p, "次の run で再判定") || o != "" {
		t.Errorf("前日以前だけなのに「次の run で再判定」と書いた: %s / %s", p, o)
	}
}
