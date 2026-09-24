package execute

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// lookupBroker は client_order_id で引ける注文だけを返す。表に無い注文は
// 「ブローカーが知らない」＝ nil を返す（応答が返らなかった注文の再現）。
type lookupBroker struct {
	broker.Broker
	orders  map[string]*domain.Order
	history []domain.Order // 当日の注文一覧（送信結果不明の突き合わせ用）
}

func (l *lookupBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	return l.orders[clientOrderID], nil
}

func (l *lookupBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	return l.history, nil
}

// sameDayLater は今から d 後の時刻。台帳の placed_at は実時計で入るので、
// 東京の日付をまたぐ（前日の注文の扱いになる）ならテストを飛ばす。
func sameDayLater(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	now := time.Now().UTC()
	later := now.Add(d)
	if clock.ToZone(now, clock.Tokyo).Format("2006-01-02") != clock.ToZone(later, clock.Tokyo).Format("2006-01-02") {
		t.Skip("東京の日付の変わり目をまたぐ")
	}
	return later
}

func openTestLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	led, err := ledger.OpenLedger(filepath.Join(t.TempDir(), "accum-test.db"))
	if err != nil {
		t.Fatalf("台帳を開けません: %v", err)
	}
	t.Cleanup(func() { led.Close() })
	return led
}

// recordPending は PENDING の注文を 1 件、台帳に入れる。
func recordPending(t *testing.T, led *ledger.Ledger, id, symbol string, qty, amount int64) domain.OrderRequest {
	t.Helper()
	price := decimal.NewFromInt(amount / qty)
	req, err := domain.NewOrderRequest(id, symbol, domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(qty), &price, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatalf("注文を作れません: %v", err)
	}
	month := "2026-08-01"
	amt := decimal.NewFromInt(amount)
	market := domain.MarketJP
	if err := led.Record(req, string(domain.OrderStatusPending), nil, &month, &amt, &market); err != nil {
		t.Fatalf("台帳に記録できません: %v", err)
	}
	return req
}

// 応答が返らず PENDING のまま残った注文は、猶予を過ぎたら当日の注文一覧と突き合わせ、
// 無ければ UNSENT（届いていない）に落とす。これをしないと、届かなかった注文が
// 永久に「発注済み」として当月の予算を食う。
func TestSyncOrderStatusRejectsStalePending(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	// 一覧は生きている（別の銘柄の注文が載っている）が、この注文は無い
	other := "999/20260904"
	b := &lookupBroker{orders: map[string]*domain.Order{}, history: []domain.Order{{
		ClientOrderID: other, BrokerOrderID: &other, Symbol: "2559", Side: domain.SideBuy,
		Trade: domain.TradeTypeCash, Quantity: decimal.NewFromInt(1), Status: domain.OrderStatusFilled,
	}}}
	later := sameDayLater(t, UnconfirmedGrace+time.Minute)

	synced, err := SyncOrderStatus(led, b, later)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 1 {
		t.Fatalf("変化の件数 = %d, want 1", len(synced.Changes))
	}
	if synced.Changes[0].After != domain.OrderStatusUnsent {
		t.Errorf("状態 = %s, want UNSENT", synced.Changes[0].After)
	}
	if synced.Resolved.NotSent != 1 {
		t.Errorf("集計 = %+v", synced.Resolved)
	}
	if !synced.Changes[0].LostAmountRatio().Equal(decimal.NewFromInt(1)) {
		t.Errorf("未約定割合 = %s, want 1", synced.Changes[0].LostAmountRatio())
	}

	// 「発注済み」から外れ、次の run で差額として埋め直される
	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, err := led.PlacedAmount("1306.T", month)
	if err != nil {
		t.Fatalf("発注済み額を引けません: %v", err)
	}
	if !placed.IsZero() {
		t.Errorf("発注済み額 = %s, want 0", placed)
	}
}

// 猶予の内側では触らない。送った直後は照会に反映されていないことがあり、
// ここで失効にすると板に残った注文と二重になる。
func TestSyncOrderStatusKeepsFreshPending(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	b := &lookupBroker{orders: map[string]*domain.Order{}}
	soon := time.Now().UTC().Add(time.Minute)

	synced, err := SyncOrderStatus(led, b, soon)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 0 {
		t.Fatalf("変化の件数 = %d, want 0", len(synced.Changes))
	}
	open, _ := led.OpenOrders()
	if len(open) != 1 || open[0].Status != string(domain.OrderStatusPending) {
		t.Errorf("PENDING のまま残っていません: %+v", open)
	}
}

// 約定した注文は「発注済み」の額を 株数 × 約定単価 に置き換える。
// 判断時の想定額のままだと、当月の残りの計算が実際に払った額とずれる。
func TestSyncOrderStatusOverwritesAmountWithFillPrice(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000) // 想定 @2500

	avg := decimal.NewFromInt(2400) // 実際は @2400 で約定
	b := &lookupBroker{orders: map[string]*domain.Order{
		req.ClientOrderID: {
			ClientOrderID:  req.ClientOrderID,
			Symbol:         "1306.T",
			Side:           domain.SideBuy,
			Quantity:       decimal.NewFromInt(100),
			FilledQuantity: decimal.NewFromInt(100),
			Status:         domain.OrderStatusFilled,
			AvgFillPrice:   &avg,
		},
	}}

	synced, err := SyncOrderStatus(led, b, time.Now().UTC())
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 1 || synced.Changes[0].After != domain.OrderStatusFilled {
		t.Fatalf("変化 = %+v", synced.Changes)
	}

	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(240_000)) {
		t.Errorf("発注済み額 = %s, want 240000", placed)
	}
}

// 一部だけ約定して失効した注文は、約定したぶんだけを「発注済み」に数える。
// amount に 株数 × 単価 を入れておき、按分は EffectiveAmount に一度だけさせる。
func TestSyncOrderStatusProratesPartialFill(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	avg := decimal.NewFromInt(2500)
	b := &lookupBroker{orders: map[string]*domain.Order{
		req.ClientOrderID: {
			ClientOrderID:  req.ClientOrderID,
			Symbol:         "1306.T",
			Side:           domain.SideBuy,
			Quantity:       decimal.NewFromInt(100),
			FilledQuantity: decimal.NewFromInt(40),
			Status:         domain.OrderStatusExpired,
			AvgFillPrice:   &avg,
		},
	}}

	if _, err := SyncOrderStatus(led, b, time.Now().UTC()); err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}

	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(100_000)) { // 40 株 × 2500
		t.Errorf("発注済み額 = %s, want 100000", placed)
	}
}

// errorBroker は照会が必ず失敗するブローカー（broker_order_id が無い注文の再現）。
type errorBroker struct {
	broker.Broker
	err error
}

func (e *errorBroker) GetOrder(string, *string) (*domain.Order, error) { return nil, e.err }
func (e *errorBroker) GetOrderHistory(time.Time, time.Time) ([]domain.Order, error) {
	return nil, e.err
}

// 当日の注文一覧を照会できないときは、勝手に失効させず保留にする。
//
// 立花証券は client_order_id を持たないので、送信結果が分からず
// broker_order_id の無い注文は一覧との突き合わせでしか決められない。
// 一覧が取れないのに UNSENT に倒すと、実は約定していた場合に翌日もう一度買ってしまう。
func TestSyncOrderStatusHoldsUnqueryableOrders(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	b := &errorBroker{err: errors.New("broker_order_id が必要です")}
	later := time.Now().UTC().Add(UnconfirmedGrace + time.Hour)

	synced, err := SyncOrderStatus(led, b, later)
	if err != nil {
		t.Fatalf("1 件の照会失敗で全体を止めるべきではありません: %v", err)
	}
	if len(synced.Changes) != 0 {
		t.Errorf("照会できていないのに台帳を変えました: %+v", synced.Changes)
	}
	if len(synced.Unresolved) != 1 {
		t.Fatalf("保留の件数 = %d, want 1", len(synced.Unresolved))
	}
	if synced.Unresolved[0].Symbol != "1306.T" {
		t.Errorf("保留 = %+v", synced.Unresolved[0])
	}

	// 台帳は動かさない（「発注済み」に数えたまま）
	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(250_000)) {
		t.Errorf("発注済み額 = %s, want 250000（保留中は動かさない）", placed)
	}
}

// 受理済みなのに注文番号の無い行（受理の応答に番号が無かった）をブローカーが知らないとき、
// 番号を辿って落ちてはいけない。保留にして次の run に回す。
func TestSyncOrderStatusHoldsUnknownSubmittedWithoutBrokerID(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000)
	if err := led.UpdateStatus(req.ClientOrderID, string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}

	b := &lookupBroker{orders: map[string]*domain.Order{}} // ブローカーは知らない
	synced, err := SyncOrderStatus(led, b, time.Now().UTC())
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 0 {
		t.Errorf("照会できていないのに台帳を変えました: %+v", synced.Changes)
	}
	if len(synced.Unresolved) != 1 || synced.Unresolved[0].ClientOrderID != req.ClientOrderID {
		t.Fatalf("保留 = %+v, want order-1 の 1 件", synced.Unresolved)
	}
}

// 送信結果不明の注文が当日の注文一覧にあれば、注文番号と約定を台帳に帰属させる。
// 人が口座を見なくても「届いていた」が分かり、二重買付にも買い漏れにもならない。
func TestSyncOrderStatusAttributesPendingFromHistory(t *testing.T) {
	led := openTestLedger(t)
	req := recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	id := "123/20260904"
	avg := decimal.NewFromInt(2450)
	created := time.Now().UTC()
	b := &lookupBroker{history: []domain.Order{{
		ClientOrderID: id, BrokerOrderID: &id, Symbol: "1306.T", Side: domain.SideBuy,
		Trade: domain.TradeTypeCash, Quantity: req.Quantity, FilledQuantity: req.Quantity,
		Status: domain.OrderStatusFilled, AvgFillPrice: &avg, CreatedAt: &created,
	}}}
	later := sameDayLater(t, UnconfirmedGrace+time.Minute)

	synced, err := SyncOrderStatus(led, b, later)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if synced.Resolved.Attributed != 1 || len(synced.Changes) != 1 || synced.Changes[0].After != domain.OrderStatusFilled {
		t.Fatalf("帰属されていない: %+v / %+v", synced.Resolved, synced.Changes)
	}
	recent, _ := led.Recent(1)
	if len(recent) != 1 || recent[0].BrokerOrderID == nil || *recent[0].BrokerOrderID != id {
		t.Errorf("注文番号が台帳に無い: %+v", recent)
	}
	month, _ := time.Parse("2006-01-02", "2026-08-01")
	placed, _ := led.PlacedAmount("1306.T", month)
	if !placed.Equal(decimal.NewFromInt(245_000)) {
		t.Errorf("発注済み額 = %s, want 245000（約定単価に置き換え）", placed)
	}
	// 一覧に載った注文はもう「知っている」ので、次の同期で別の PENDING に帰属しない
	recordPending(t, led, "order-2", "1306.T", 100, 250_000)
	synced, _ = SyncOrderStatus(led, b, later)
	if synced.Resolved.NotSent != 1 {
		t.Errorf("既知の注文番号が二重に帰属された: %+v", synced.Resolved)
	}
}

// 前日以前に送った送信結果不明の注文は、今日の一覧に無くても UNSENT にしない（A1）。
// 立花の注文一覧が当日分しか返さなければ、前日に約定した注文も「無い」と見える。
// UNSENT にすると次の run が同じ額をもう一度買う。
func TestSyncOrderStatusHoldsPendingFromEarlierDay(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	other := "999/20260905"
	b := &lookupBroker{orders: map[string]*domain.Order{}, history: []domain.Order{{
		ClientOrderID: other, BrokerOrderID: &other, Symbol: "2559", Side: domain.SideBuy,
		Trade: domain.TradeTypeCash, Quantity: decimal.NewFromInt(1), Status: domain.OrderStatusSubmitted,
	}}}
	nextDay := time.Now().UTC().Add(24 * time.Hour)

	synced, err := SyncOrderStatus(led, b, nextDay)
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 0 || synced.Resolved.NotSent != 0 {
		t.Fatalf("前日の注文を一覧に無いだけで確定した: %+v / %+v", synced.Changes, synced.Resolved)
	}
	if len(synced.Unresolved) != 1 || !strings.Contains(synced.Unresolved[0].Reason, "今日より前") {
		t.Fatalf("保留として知らせていない: %+v", synced.Unresolved)
	}
	open, err := led.OpenOrders()
	if err != nil || len(open) != 1 || open[0].Status != string(domain.OrderStatusPending) {
		t.Errorf("PENDING のまま残っていない: %+v (err: %v)", open, err)
	}
	pending, err := led.PendingSymbols()
	if err != nil || len(pending["1306.T"]) != 1 {
		t.Errorf("発注を止める銘柄に数えられていない: %+v (err: %v)", pending, err)
	}
}

// 当日の一覧が 0 件なら判定しない。一覧が空で返ったのか届いていないのか区別できない。
func TestSyncOrderStatusHoldsWhenListEmpty(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)

	b := &lookupBroker{orders: map[string]*domain.Order{}}
	synced, err := SyncOrderStatus(led, b, sameDayLater(t, UnconfirmedGrace+time.Minute))
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if len(synced.Changes) != 0 {
		t.Fatalf("空の一覧で確定した: %+v", synced.Changes)
	}
	if len(synced.Unresolved) != 1 || !strings.Contains(synced.Unresolved[0].Reason, "0 件") {
		t.Fatalf("保留として知らせていない: %+v", synced.Unresolved)
	}
}

// 今日送って注文番号の分かっている注文が一覧に無ければ、一覧を信用せず判定を先送りする
// （Expected。以前は渡しておらず、欠けた一覧で UNSENT にしていた）。
func TestSyncOrderStatusDefersWhenExpectedOrderMissing(t *testing.T) {
	led := openTestLedger(t)
	recordPending(t, led, "order-1", "1306.T", 100, 250_000)
	known := "555/20260905"
	req := newRequest(t, "order-0", "2559", 1)
	month, amount, market := "2026-09-01", decimal.NewFromInt(3000), domain.MarketJP
	if err := led.Record(req, string(domain.OrderStatusSubmitted), &known, &month, &amount, &market); err != nil {
		t.Fatal(err)
	}

	// 一覧には関係の無い注文だけ（今日の 555 が載っていない＝欠けた一覧）
	other := "999/20260905"
	b := &lookupBroker{
		orders: map[string]*domain.Order{"order-0": {ClientOrderID: "order-0", BrokerOrderID: &known,
			Symbol: "2559", Side: domain.SideBuy, Quantity: decimal.NewFromInt(1), Status: domain.OrderStatusSubmitted}},
		history: []domain.Order{{
			ClientOrderID: other, BrokerOrderID: &other, Symbol: "1629", Side: domain.SideBuy,
			Trade: domain.TradeTypeCash, Quantity: decimal.NewFromInt(10), Status: domain.OrderStatusSubmitted,
		}},
	}
	synced, err := SyncOrderStatus(led, b, sameDayLater(t, UnconfirmedGrace+time.Minute))
	if err != nil {
		t.Fatalf("照会に失敗: %v", err)
	}
	if synced.Resolved.NotSent != 0 || synced.Resolved.TooRecent != 1 {
		t.Fatalf("欠けた一覧で判定した: %+v", synced.Resolved)
	}
	pending, _ := led.PendingSymbols()
	if len(pending["1306.T"]) != 1 {
		t.Errorf("PENDING のまま残っていない: %+v", pending)
	}
}
