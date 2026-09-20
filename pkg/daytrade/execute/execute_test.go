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
	posErr     error // 現物の照会エラー
	marginErr  error // 信用建玉の照会エラー（現物とは別の電文なので別に持つ）
	balance    domain.Balance
	balanceErr error
	placed     []domain.OrderRequest
	history    []domain.Order // 当日の注文一覧
	historyErr error
	cancel     func(clientOrderID string) error
	cancelled  []string // 取消を送った client_order_id
}

func (s *stubBroker) Name() string      { return "stub" }
func (s *stubBroker) AccountID() string { return "stub" }
func (s *stubBroker) GetBalance() (*domain.Balance, error) {
	if s.balanceErr != nil {
		return nil, s.balanceErr
	}
	b := s.balance
	return &b, nil
}

// GetPositions は現物だけ、MarginPositions は信用だけ（立花証券と同じ切り分け）。
func (s *stubBroker) GetPositions() ([]domain.Position, error) {
	return s.positionsOfKind(false), s.posErr
}

func (s *stubBroker) MarginPositions() ([]domain.Position, error) {
	return s.positionsOfKind(true), s.marginErr
}

func (s *stubBroker) positionsOfKind(margin bool) []domain.Position {
	var out []domain.Position
	for _, p := range s.positions {
		if broker.LegOf(p.Symbol, p.Trade, p.Quantity.IsNegative()).Margin == margin {
			out = append(out, p)
		}
	}
	return out
}
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
func (s *stubBroker) Cancel(clientOrderID string, _ *string) error {
	s.cancelled = append(s.cancelled, clientOrderID)
	if s.cancel == nil {
		return nil
	}
	return s.cancel(clientOrderID)
}
func (s *stubBroker) LotSizes(_ []string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{}
}

// recorder は Reporter の模型。通知と Error ログを覚える。
type recorder struct {
	alerts   []string
	errors   []string
	warnings []string
}

func (r *recorder) Info(string, string, ...map[string]any) {}
func (r *recorder) Warn(code, msg string, _ ...map[string]any) {
	r.warnings = append(r.warnings, code+": "+msg)
}

// warned はその符号の警告が出たか。
func (r *recorder) warned(code string) bool {
	for _, w := range r.warnings {
		if strings.HasPrefix(w, code+": ") {
			return true
		}
	}
	return false
}
func (r *recorder) Error(code, msg string, _ ...map[string]any) {
	r.errors = append(r.errors, code+": "+msg)
}
func (r *recorder) Alert(title, body string) { r.alerts = append(r.alerts, title+" | "+body) }

var day = time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)

// pricedBroker は時価を返せるブローカー（broker.PriceSource）。本番では立花だけが満たす。
type pricedBroker struct {
	*stubBroker
	prices    map[string]broker.MarketPrice
	pricesErr error
	asked     []string // 時価を聞いた銘柄
	calls     int      // 時価問合の回数（まとめ取りが効いていれば発注ループごとに 1 回）
}

func (p *pricedBroker) MarketPrices(symbols []string) (map[string]broker.MarketPrice, error) {
	p.calls++
	p.asked = append(p.asked, symbols...)
	if p.pricesErr != nil {
		return nil, p.pricesErr
	}
	return p.prices, nil
}

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

// statusOf は台帳に入った注文を引く。引く日は **env.Day**——固定の day を見ると、
// 判定日を今日にずらす試験（todayEnv）が「今日 ≠ day」の日に落ちる。
func statusOf(t *testing.T, env Env, symbol string) ledger.Order {
	t.Helper()
	entries, err := env.Ledger.EntriesOn(env.Day)
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

// 寄る前の回（Env.Preopen）は preopen_legs の脚を寄成でブローカーに送り、台帳にも残す。
// 台帳の condition を見れば、あとから寄成とザラ場の成行の滑りを分けて測れる。
func TestPlacePicksPreopenSendsOpeningCondition(t *testing.T) {
	env, _ := newEnv(t)
	env.Cfg.Margin.Enabled = true
	env.Cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	env.Cfg.Execution.PreopenLegs = config.PreopenLegsBoth
	env.Preopen = true
	b := &stubBroker{balance: richBalance()}

	if _, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil || len(failures) != 0 {
		t.Fatalf("failures=%v err=%v", failures, err)
	}
	if len(b.placed) != 1 || b.placed[0].Condition != domain.ConditionOpening {
		t.Fatalf("送った注文の執行条件 = %q, want %q", b.placed[0].Condition, domain.ConditionOpening)
	}
	if b.placed[0].OrderType != domain.OrderTypeMarket {
		t.Errorf("寄成は成行のまま: %s", b.placed[0].OrderType)
	}
	if o := statusOf(t, env, "7203"); o.Condition != domain.ConditionOpening {
		t.Errorf("台帳の執行条件 = %q, want %q", o.Condition, domain.ConditionOpening)
	}

	// 9:00 以降の回は同じ設定でもザラ場の成行（台帳も空のまま）
	env2, _ := newEnv(t)
	env2.Cfg = env.Cfg
	env2.Preopen = false
	b2 := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env2, b2, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	if len(b2.placed) != 1 || b2.placed[0].Condition != domain.ConditionNone {
		t.Errorf("9:00 以降の執行条件 = %q, want 空", b2.placed[0].Condition)
	}
	if o := statusOf(t, env2, "7203"); o.Condition != domain.ConditionNone {
		t.Errorf("台帳の執行条件 = %q, want 空", o.Condition)
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

// TestPlacePicksBalanceFailureSkipsOnlyThatPick は、余力を照会できなくても実行ごと止めず、
// その銘柄だけ見送って次へ進むこと（止めると残りの銘柄がその日二度と建たない）。
func TestPlacePicksBalanceFailureSkipsOnlyThatPick(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balanceErr: errors.New("timeout")}
	picks := []selection.Pick{pick("7203", domain.SideBuy), pick("9984", domain.SideBuy)}
	orders, failures, err := PlacePicks(env, b, picks)
	if err != nil {
		t.Fatalf("余力照会の失敗で実行が止まった: %v", err)
	}
	if orders != 0 || len(failures) != 2 || len(b.placed) != 0 {
		t.Fatalf("orders=%d failures=%v placed=%d", orders, failures, len(b.placed))
	}
	// 復旧すれば同じ判断で建てられる（台帳に何も残していない）
	b.balanceErr, b.balance = nil, richBalance()
	if orders, _, _ = PlacePicks(env, b, picks); orders != 2 {
		t.Errorf("復旧後に建てられない: orders=%d", orders)
	}
}

// TestPlacePicksStopsAtDeadline は、締め切りを過ぎたら新しい注文を送らず「締め切り」として
// 見送ること。送らなかった分は台帳に残らないので、次の回が建て直せる。
func TestPlacePicksStopsAtDeadline(t *testing.T) {
	env, _ := newEnv(t)
	env.Deadline = time.Now().Add(-time.Second)
	b := &stubBroker{balance: richBalance()}
	orders, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || orders != 0 || len(b.placed) != 0 {
		t.Fatalf("締め切り後に送った: orders=%d placed=%d err=%v", orders, len(b.placed), err)
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "締め切り") {
		t.Errorf("見送りの理由が締め切りになっていない: %v", failures)
	}
	if entries, _ := env.Ledger.EntriesOn(env.Day); len(entries) != 0 {
		t.Errorf("送っていない注文が台帳に残った: %+v", entries)
	}
	// dry-run は締め切りに縛られない（判断の記録は残す）
	if orders, _, _ = PlacePicks(env, nil, []selection.Pick{pick("7203", domain.SideBuy)}); orders != 1 {
		t.Errorf("dry-run が締め切りで止まった: orders=%d", orders)
	}
}

// TestPlaceRecordedDeadlineFromBrokerIsUnsent は、ブローカーが「締め切りで送らなかった」と
// 返したら PENDING ではなく UNSENT にすること（送っていないので照会で判定する必要が無い）。
// 執行の滑りは「送る直前の時価」と約定の差でしか測れない。判断に使った 9:00 の気配
// （Price）との差には、発注までの遅れが混じる。両方を台帳に残すこと。
func TestPlaceRecordsRefPrice(t *testing.T) {
	env, _ := newEnv(t)
	at := time.Date(2026, 9, 16, 0, 4, 0, 0, time.UTC)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()}, prices: map[string]broker.MarketPrice{
		"7203": {
			Symbol: "7203", Last: decimal.NewFromInt(1020),
			Bid: decimal.NewFromInt(1019), Ask: decimal.NewFromInt(1021), At: at,
		},
	}}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	o := statusOf(t, env, "7203")
	if o.RefPrice == nil || !o.RefPrice.Equal(decimal.NewFromInt(1020)) {
		t.Errorf("執行時の時価 = %v, want 1020", o.RefPrice)
	}
	if o.RefBid == nil || o.RefAsk == nil {
		t.Errorf("最良気配が残っていない: %v / %v", o.RefBid, o.RefAsk)
	}
	if o.Price == nil || !o.Price.Equal(decimal.NewFromInt(1000)) {
		t.Errorf("判断時の気配 = %v, want 1000（遅れと滑りを分けるため両方残す）", o.Price)
	}
	if len(b.asked) != 1 || b.asked[0] != "7203" {
		t.Errorf("送る直前の時価を聞くこと: %v", b.asked)
	}
}

// 現在値の無い（まだ寄っていない）銘柄は最良気配の仲値で控える。
func TestPlaceRefFallsBackToBook(t *testing.T) {
	env, _ := newEnv(t)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()}, prices: map[string]broker.MarketPrice{
		"7203": {Symbol: "7203", Bid: decimal.NewFromInt(1000), Ask: decimal.NewFromInt(1010)},
	}}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	if o := statusOf(t, env, "7203"); o.RefPrice == nil || !o.RefPrice.Equal(decimal.NewFromInt(1005)) {
		t.Errorf("仲値 = %v, want 1005", o.RefPrice)
	}
}

// 取ったばかりの気配を渡された回は時価を取り直さず、その値を台帳に控える
// （順位表と 1 本目の注文の間の往復を省く）。足りない銘柄があれば従来どおり取り直す。
func TestPlacePicksReusesFreshQuotes(t *testing.T) {
	env, _ := newEnv(t)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()}, prices: map[string]broker.MarketPrice{
		"7203": {Symbol: "7203", Last: decimal.NewFromInt(999)},
		"6758": {Symbol: "6758", Last: decimal.NewFromInt(888)},
	}}
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	quotes := map[string]selection.Quote{
		"7203": {Symbol: "7203", Price: decimal.NewFromInt(1000), Bid: decimal.NewFromInt(1000), Ask: decimal.NewFromInt(1010)},
		"6758": {Symbol: "6758", Price: decimal.NewFromInt(2000)}, // 板も現在値も無い（CSV の取得元）
	}
	env.RefPrices = RefPricesFromQuotes(quotes, picks)
	if _, _, err := PlacePicks(env, b, picks); err != nil {
		t.Fatal(err)
	}
	if b.calls != 0 {
		t.Errorf("気配を渡したのに時価を %d 回取り直した", b.calls)
	}
	if o := statusOf(t, env, "7203"); o.RefPrice == nil || !o.RefPrice.Equal(decimal.NewFromInt(1005)) {
		t.Errorf("控えた時価 = %v, want 1005（渡した気配の仲値）", o.RefPrice)
	}

	more := []selection.Pick{pick("6758", domain.SideBuy)}
	if got := RefPricesFromQuotes(quotes, more); got != nil {
		t.Errorf("板も現在値も無い気配を時価として渡した: %v", got)
	}
	env.RefPrices = RefPricesFromQuotes(quotes, picks) // 6758 を含まない
	if _, _, err := PlacePicks(env, b, more); err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 {
		t.Errorf("足りない銘柄があるのに取り直していない（%d 回）", b.calls)
	}
	if o := statusOf(t, env, "6758"); o.RefPrice == nil || !o.RefPrice.Equal(decimal.NewFromInt(888)) {
		t.Errorf("取り直した時価 = %v, want 888", o.RefPrice)
	}
}

// 時価は記録のためだけの値。取れなくても注文は出す（測れないより買い漏れの方が高くつく）。
func TestPlaceContinuesWhenRefPriceFails(t *testing.T) {
	env, rep := newEnv(t)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()},
		pricesErr: errors.New("時価問合に失敗")}
	orders, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || orders != 1 || len(failures) != 0 {
		t.Fatalf("orders=%d failures=%v err=%v", orders, failures, err)
	}
	if o := statusOf(t, env, "7203"); o.RefPrice != nil {
		t.Errorf("取れなかった時価が入っている: %v", o.RefPrice)
	}
	if !rep.warned("daytrade.ref_price") {
		t.Error("時価を取れなかったことを警告していない")
	}
}

// まとめ取りが落ちた回は、注文ごとに 1 銘柄ずつ聞き直さない。聞き直すと失敗する往復が
// 注文の間に直列で挟まり、往復がまとめ取り導入前より増える——引けなら 15:20〜15:30 の
// 締め切りを削り、手仕舞いを送れずに持ち越しへ化けうる（2026-09-17 のレビュー）。
func TestPlaceDoesNotRetryPerSymbolWhenBatchFails(t *testing.T) {
	env, _ := newEnv(t)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()},
		pricesErr: errors.New("時価問合に失敗")}
	picks := []selection.Pick{pick("7203", domain.SideBuy), pick("6758", domain.SideBuy)}
	orders, failures, err := PlacePicks(env, b, picks)
	if err != nil || orders != 2 || len(failures) != 0 {
		t.Fatalf("orders=%d failures=%v err=%v", orders, failures, err)
	}
	if b.calls != 1 {
		t.Errorf("時価問合 %d 回, want 1（まとめ取りの 1 回だけ。銘柄ごとに聞き直さない）", b.calls)
	}
}

func TestPlaceRecordedDeadlineFromBrokerIsUnsent(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, &broker.ErrDeadline{CLMID: "CLMKabuNewOrder", Deadline: time.Now()}
	}
	_, failures, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if len(failures) != 1 {
		t.Fatalf("failures=%v", failures)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusUnsent) {
		t.Errorf("締め切りの未送信が UNSENT になっていない: %s", o.Status)
	}
	// 次の回は種を変えて送れる
	b.place = nil
	if orders, _, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); orders != 1 {
		t.Errorf("未送信の後に建てられない: orders=%d", orders)
	}
}

// TestPlaceRecordedNotSentFromBrokerIsUnsent は、ブローカーが「送る前に失敗した」
// （返済する建玉の照会が落ちた等）と返したら、結果不明（PENDING）ではなく UNSENT にすること。
// 届いた可能性が無いのに PENDING にすると、一覧照会で判定するまで再送できない。
func TestPlaceRecordedNotSentFromBrokerIsUnsent(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, &broker.ErrNotSent{ClientOrderID: req.ClientOrderID, Err: errors.New("建玉を照会できません")}
	}
	_, failures, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if len(failures) != 1 {
		t.Fatalf("failures=%v", failures)
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusUnsent) {
		t.Errorf("送る前の失敗が UNSENT になっていない: %s", o.Status)
	}
	b.place = nil
	if orders, _, _ := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); orders != 1 {
		t.Errorf("未送信の後に建てられない: orders=%d", orders)
	}
}

// TestPlacedTodayCountsLiveEntriesOnly は、建玉の数に dry-run と拒否・失効を数えないこと。
func TestPlacedTodayCountsLiveEntriesOnly(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	if _, _, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy), pick("9984", domain.SideSell)}); err != nil {
		t.Fatal(err)
	}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		return nil, &broker.OrderRejectedError{Message: "x"}
	}
	_, _, _ = PlacePicks(env, b, []selection.Pick{pick("6758", domain.SideBuy)})
	_, _, _ = PlacePicks(env, nil, []selection.Pick{pick("8306", domain.SideBuy)})

	placed, err := PlacedToday(env)
	if err != nil {
		t.Fatal(err)
	}
	if placed.Long != 1 || placed.Short != 1 || placed.Total() != 2 {
		t.Errorf("long=%d short=%d", placed.Long, placed.Short)
	}
	if placed.Symbols["7203"] != domain.SideBuy || placed.Symbols["9984"] != domain.SideSell {
		t.Errorf("symbols=%v", placed.Symbols)
	}
	if _, ok := placed.Symbols["6758"]; ok {
		t.Error("拒否された銘柄が建玉に数えられた")
	}
}

func TestEntryRequestUsesMarginWhenConfigured(t *testing.T) {
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Margin.LongViaMargin = true
	long := EntryRequest(pick("7203", domain.SideBuy), day, cfg, 0, false)
	short := EntryRequest(pick("9984", domain.SideSell), day, cfg, 0, false)
	if long.Trade != domain.TradeTypeMarginOpen || short.Trade != domain.TradeTypeMarginOpen {
		t.Errorf("信用: long=%s short=%s", long.Trade, short.Trade)
	}
	if EntryRequest(pick("7203", domain.SideBuy), day, config.Default(), 0, false).Trade != domain.TradeTypeCash {
		t.Error("既定は現物")
	}
}

// 寄成になるのは「寄る前の回 × その脚が preopen_legs に挙がっている」ときだけ。
// 9:00 以降の回（preopen = false）は設定にかかわらず従来のザラ場の成行。
func TestEntryRequestOpeningCondition(t *testing.T) {
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	cases := []struct {
		legs    string
		preopen bool
		long    domain.OrderCondition
		short   domain.OrderCondition
	}{
		{config.PreopenLegsNone, true, domain.ConditionNone, domain.ConditionNone},
		{config.PreopenLegsShort, true, domain.ConditionNone, domain.ConditionOpening},
		{config.PreopenLegsLong, true, domain.ConditionOpening, domain.ConditionNone},
		{config.PreopenLegsBoth, true, domain.ConditionOpening, domain.ConditionOpening},
		// 寄ってからの回は寄成にしない
		{config.PreopenLegsBoth, false, domain.ConditionNone, domain.ConditionNone},
	}
	for _, c := range cases {
		cfg.Execution.PreopenLegs = c.legs
		if err := cfg.Validate(); err != nil {
			t.Fatalf("legs=%s: %v", c.legs, err)
		}
		long := EntryRequest(pick("7203", domain.SideBuy), day, cfg, 0, c.preopen)
		short := EntryRequest(pick("9984", domain.SideSell), day, cfg, 0, c.preopen)
		if long.Condition != c.long || short.Condition != c.short {
			t.Errorf("legs=%s preopen=%v: long=%q short=%q, want long=%q short=%q",
				c.legs, c.preopen, long.Condition, short.Condition, c.long, c.short)
		}
		// 寄成でも成行のまま（値段は付かない）
		if long.OrderType != domain.OrderTypeMarket || long.LimitPrice != nil {
			t.Errorf("legs=%s: 種別 %s 値段 %v", c.legs, long.OrderType, long.LimitPrice)
		}
	}
}

func TestEnsureNoUnrecordedPositions(t *testing.T) {
	env, rep := newEnv(t)
	picks := []selection.Pick{pick("7203", domain.SideBuy)}
	b := &stubBroker{positions: []domain.Position{{Symbol: "7203", Quantity: decimal.NewFromInt(100)}}}

	err := EnsureNoUnrecordedPositions(env, broker.PositionsByLeg(b), picks, nil)
	var unrecorded *ErrUnrecordedPositions
	if !errors.As(err, &unrecorded) || len(unrecorded.Positions) != 1 {
		t.Fatalf("台帳外の建玉で止まらない: %v", err)
	}
	if len(rep.alerts) != 1 {
		t.Errorf("通知が出ていない: %v", rep.alerts)
	}

	// 台帳が今日の建玉として知っていれば止めない（正常な再実行）
	req := EntryRequest(picks[0], day, env.Cfg, 0, false)
	if err := env.Ledger.Record(req, day, string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := EnsureNoUnrecordedPositions(env, broker.PositionsByLeg(b), picks, nil); err != nil {
		t.Errorf("台帳が知っている建玉で止まった: %v", err)
	}

	// 照会できないときも止める
	b.posErr = errors.New("down")
	if err := EnsureNoUnrecordedPositions(env, broker.PositionsByLeg(b), picks, nil); err == nil {
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

// TestPlaceExitsPrefetchesRefPrices は、引けの手仕舞いも時価を**まとめて 1 回で**取ること。
// 1 銘柄ずつ聞くと注文ごとに同期の往復が直列で挟まり、15:20〜15:30 の締め切りをそのぶん
// 削る（2026-09-16 のレビュー）。
func TestPlaceExitsPrefetchesRefPrices(t *testing.T) {
	env, _ := newEnv(t)
	b := &pricedBroker{stubBroker: &stubBroker{balance: richBalance()}, prices: map[string]broker.MarketPrice{
		"7203": {Symbol: "7203", Last: decimal.NewFromInt(1020)},
		"9984": {Symbol: "9984", Last: decimal.NewFromInt(2030)},
	}}
	if _, _, err := PlacePicks(env, b, []selection.Pick{
		pick("7203", domain.SideBuy), pick("9984", domain.SideBuy)}); err != nil {
		t.Fatal(err)
	}
	price := decimal.NewFromInt(990)
	b.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
			Quantity: decimal.NewFromInt(100), FilledQuantity: decimal.NewFromInt(100), AvgFillPrice: &price}, nil
	}
	entries, _, err := LiveEntries(env)
	if err != nil {
		t.Fatal(err)
	}
	targets, _, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	// 建てのぶんは数えない。ここから先が手仕舞いの問合
	b.calls, b.asked = 0, nil
	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatalf("手仕舞い: %v", failures)
	}
	if b.calls != 1 {
		t.Errorf("時価問合 %d 回（2 銘柄を 1 回でまとめて取ること）: %v", b.calls, b.asked)
	}
	if len(b.asked) != 2 {
		t.Errorf("聞いた銘柄 = %v, want 7203 と 9984", b.asked)
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

// 締め切りで待ちを縮めた回は、その場で一覧を見ない。受付が一覧に載る前に見ると
// 「届いていない」と読んで種を変えて送り直す（二重発注）。判定は次の実行に回す。
func TestUnconfirmedDefersWhenDeadlineShortensWait(t *testing.T) {
	env, _ := todayEnv(t)
	env.RetryWait = time.Hour
	env.Deadline = time.Now().Add(100 * time.Millisecond)
	b := &stubBroker{balance: richBalance()} // 一覧は空（まだ載っていない）
	b.place = func(domain.OrderRequest) (*domain.OrderAck, error) { return nil, errors.New("timeout") }
	_, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)})
	if err != nil || len(failures) != 1 {
		t.Fatalf("failures=%v err=%v", failures, err)
	}
	if len(b.placed) != 1 {
		t.Errorf("待ちきれないのに送り直した: %d", len(b.placed))
	}
	if o := statusOf(t, env, "7203"); o.Status != string(domain.OrderStatusPending) {
		t.Errorf("次の実行に渡すため PENDING のまま: %s", o.Status)
	}
}

// 一覧が空で返っても、今日送って注文番号の分かっている注文があるなら一覧の方を疑う。
func TestUnconfirmedStaysPendingWhenListLacksKnownOrders(t *testing.T) {
	env, _ := todayEnv(t)
	b := &stubBroker{balance: richBalance()}
	picks := []selection.Pick{pick("7203", domain.SideBuy), pick("9984", domain.SideBuy)}
	b.place = func(req domain.OrderRequest) (*domain.OrderAck, error) {
		if req.Symbol == "9984" {
			return nil, errors.New("timeout")
		}
		id := "N/" + req.Symbol
		return &domain.OrderAck{ClientOrderID: req.ClientOrderID, BrokerOrderID: &id, Status: domain.OrderStatusSubmitted}, nil
	}
	_, failures, err := PlacePicks(env, b, picks)
	if err != nil || len(failures) != 1 {
		t.Fatalf("failures=%v err=%v", failures, err)
	}
	if len(b.placed) != 2 {
		t.Errorf("空の一覧を信用して送り直した: placed=%d", len(b.placed))
	}
	if o := statusOf(t, env, "9984"); o.Status != string(domain.OrderStatusPending) {
		t.Errorf("PENDING のまま: %s", o.Status)
	}
}

// 台帳で確定済みの建玉（約定・拒否・未送信）はブローカーに聞かない。
// 拒否・未送信は注文番号が無く、聞くと毎回エラーの警告になる。
func TestRefreshEntriesDoesNotQueryFinalizedOrders(t *testing.T) {
	env, _ := newEnv(t)
	price := decimal.NewFromInt(990)
	id := "B/filled"
	for _, c := range []struct {
		symbol string
		status domain.OrderStatus
	}{{"7203", domain.OrderStatusFilled}, {"9984", domain.OrderStatusRejected}, {"6758", domain.OrderStatusUnsent}} {
		req := EntryRequest(pick(c.symbol, domain.SideBuy), env.Day, env.Cfg, 0, false)
		if err := env.Ledger.Record(req, env.Day, string(domain.OrderStatusSubmitted), &price, nil); err != nil {
			t.Fatal(err)
		}
		filled, fill, broker := decimal.Zero, (*decimal.Decimal)(nil), (*string)(nil)
		if c.status == domain.OrderStatusFilled {
			filled, fill, broker = decimal.NewFromInt(100), &price, &id
		}
		if err := env.Ledger.UpdateStatus(req.ClientOrderID, c.status, filled, fill, broker); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	b := &stubBroker{getOrder: func(string) (*domain.Order, error) {
		calls++
		return nil, errors.New("注文番号がありません")
	}}
	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 0 {
		t.Fatalf("unconfirmed=%v err=%v", unconfirmed, err)
	}
	if calls != 0 {
		t.Errorf("確定済みの注文をブローカーに聞いた: %d 回", calls)
	}
	if len(targets) != 1 || targets[0].Entry.Symbol != "7203" || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Errorf("約定済みの 7203 だけを台帳の値で手仕舞うはず: %+v", targets)
	}
}

// exitRerun は「15:20 に手仕舞いが受理された」ところまで進め、その返済注文の ID を返す。
// 2 回目（15:24）の照会の応答は呼び出し側が b.getOrder で決める。
func exitRerun(t *testing.T, env Env, b *stubBroker) (entryID, exitID string) {
	t.Helper()
	entryID = recordLongToday(t, env, "7203", 100)
	price := decimal.NewFromInt(761)
	b.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusFilled,
			Quantity: decimal.NewFromInt(100), FilledQuantity: decimal.NewFromInt(100), AvgFillPrice: &price}, nil
	}
	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 1 {
		t.Fatalf("1 回目: targets=%+v err=%v", targets, err)
	}
	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatalf("1 回目の手仕舞い: %v", failures)
	}
	return entryID, b.placed[len(b.placed)-1].ClientOrderID
}

// 15:20 に受理された返済が、その後ブローカー側で拒否・失効していたら、次の回（15:24）が送り直すこと。
// 台帳の送信済みを信じて「発注済み（冪等）」と数えると、返済が無いまま持ち越す。
func TestRefreshEntriesResendsExitThatDiedAfterAcceptance(t *testing.T) {
	env, _ := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	_, exitID := exitRerun(t, env, b)
	sent := len(b.placed)

	b.getOrder = func(id string) (*domain.Order, error) {
		if id != exitID {
			t.Errorf("確定済みの建て注文を聞き直した: %s", id)
		}
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusRejected,
			Quantity: decimal.NewFromInt(100)}, nil
	}
	entries, _, _ := LiveEntries(env)
	targets, unconfirmed, err := RefreshEntries(env, b, entries)
	if err != nil || len(unconfirmed) != 0 || len(targets) != 1 || !targets[0].Quantity.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("2 回目: targets=%+v unconfirmed=%v err=%v, want 100 株を送り直す", targets, unconfirmed, err)
	}
	if o, _, _ := env.Ledger.Get(exitID); o.Status != string(domain.OrderStatusRejected) {
		t.Errorf("1 回目の返済の台帳 = %s, want REJECTED", o.Status)
	}
	if failures := PlaceExits(env, b, targets); len(failures) != 0 {
		t.Fatalf("2 回目の手仕舞い: %v", failures)
	}
	if len(b.placed) != sent+1 || b.placed[len(b.placed)-1].ClientOrderID == exitID {
		t.Errorf("送り直していない（または同じ ID）: placed=%d → %d", sent, len(b.placed))
	}
}

// 返済がまだ板に生きている・照会できないときは、もう 1 本重ねない（二重の返済にしない）。
func TestRefreshEntriesDoesNotDoubleLiveOrUnknownExit(t *testing.T) {
	for name, reply := range map[string]func(id string) (*domain.Order, error){
		"板に生きている": func(id string) (*domain.Order, error) {
			return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusSubmitted,
				Quantity: decimal.NewFromInt(100)}, nil
		},
		"照会できない": func(string) (*domain.Order, error) { return nil, errors.New("timeout") },
	} {
		t.Run(name, func(t *testing.T) {
			env, _ := newEnv(t)
			b := &stubBroker{balance: richBalance()}
			exitRerun(t, env, b)
			b.getOrder = reply
			entries, _, _ := LiveEntries(env)
			targets, _, err := RefreshEntries(env, b, entries)
			if err != nil || len(targets) != 0 {
				t.Fatalf("targets=%+v err=%v, want 対象なし", targets, err)
			}
		})
	}
}

// 一部約定して終わった返済が、聞き直しで約定 0 の終了状態に見えても、台帳の約定分を消さない。
// 消すと死んだ注文と数えられ、返済済みの株数まで送り直す（反対建玉）。
func TestRefreshEntriesKeepsExitFillWhenLookupShrinks(t *testing.T) {
	env, rep := newEnv(t)
	b := &stubBroker{balance: richBalance()}
	_, exitID := exitRerun(t, env, b)
	if err := env.Ledger.UpdateStatus(exitID, domain.OrderStatusPartiallyFilled, decimal.NewFromInt(60), nil, nil); err != nil {
		t.Fatal(err)
	}
	b.getOrder = func(id string) (*domain.Order, error) {
		return &domain.Order{ClientOrderID: id, Status: domain.OrderStatusExpired,
			Quantity: decimal.NewFromInt(100)}, nil
	}
	entries, _, _ := LiveEntries(env)
	targets, _, err := RefreshEntries(env, b, entries)
	if err != nil || len(targets) != 0 || !rep.warned("daytrade.exit_refresh") {
		t.Fatalf("targets=%+v err=%v, want 対象なしと警告", targets, err)
	}
	if o, _, _ := env.Ledger.Get(exitID); !o.FilledQuantity.Equal(decimal.NewFromInt(60)) {
		t.Errorf("台帳の約定数量 = %s, want 60 のまま", o.FilledQuantity)
	}
}

// preopen_limit_pct が正なら、寄る前の回のロングは寄指（指値 × 寄付）になる。指値は
// 前日終値 × (1 − x%) を呼値に切り下げた値。ショート・寄った後の回・前日終値なしは従来のまま。
func TestEntryRequestOpeningLimit(t *testing.T) {
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	cfg.Execution.PreopenLegs = config.PreopenLegsBoth
	cfg.Execution.PreopenLimitPct = decimal.RequireFromString("0.5")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	long := EntryRequest(pick("7203", domain.SideBuy), day, cfg, 0, true)
	// 1030 × 0.995 = 1024.85 → 1 円刻みで切り下げ
	if long.OrderType != domain.OrderTypeLimit || long.LimitPrice == nil || !long.LimitPrice.Equal(decimal.NewFromInt(1024)) {
		t.Fatalf("寄指の種別・指値 = %s / %v, want LIMIT / 1024", long.OrderType, long.LimitPrice)
	}
	if long.Condition != domain.ConditionOpening || !strings.Contains(long.Reason, "寄指 1024") {
		t.Errorf("執行条件 = %q、理由 = %q", long.Condition, long.Reason)
	}

	short := EntryRequest(pick("9984", domain.SideSell), day, cfg, 0, true)
	if short.OrderType != domain.OrderTypeMarket || short.LimitPrice != nil || short.Condition != domain.ConditionOpening {
		t.Errorf("ショートは寄成のまま: %s / %v / %q", short.OrderType, short.LimitPrice, short.Condition)
	}
	later := EntryRequest(pick("7203", domain.SideBuy), day, cfg, 0, false)
	if later.OrderType != domain.OrderTypeMarket || later.LimitPrice != nil || later.Condition != domain.ConditionNone {
		t.Errorf("寄った後の回はザラ場の成行のまま: %s / %v / %q", later.OrderType, later.LimitPrice, later.Condition)
	}
	noPrev := pick("7203", domain.SideBuy)
	noPrev.PrevClose = decimal.Zero
	fallback := EntryRequest(noPrev, day, cfg, 0, true)
	if fallback.OrderType != domain.OrderTypeMarket || fallback.LimitPrice != nil || fallback.Condition != domain.ConditionOpening {
		t.Errorf("前日終値が無ければ寄成で出す: %s / %v / %q", fallback.OrderType, fallback.LimitPrice, fallback.Condition)
	}
}

// 指値に届かず失効した寄指は使った枠に数える（後の回が成行で埋め直さない）。拒否は従来どおり埋め直す。
func TestPlacedTodayCountsLapsedOpeningAsUsed(t *testing.T) {
	env, _ := newEnv(t)
	env.Cfg.Margin.Enabled = true
	env.Cfg.Execution.EntryWindow = []string{"08:59", "09:15"}
	env.Cfg.Execution.PreopenLegs = config.PreopenLegsLong
	env.Cfg.Execution.PreopenLimitPct = decimal.RequireFromString("0.5")
	env.Preopen = true
	b := &stubBroker{balance: richBalance()}
	if _, failures, err := PlacePicks(env, b, []selection.Pick{pick("7203", domain.SideBuy)}); err != nil || len(failures) != 0 {
		t.Fatalf("failures=%v err=%v", failures, err)
	}
	if b.placed[0].OrderType != domain.OrderTypeLimit || b.placed[0].Condition != domain.ConditionOpening {
		t.Fatalf("送った注文 = %s / %q, want LIMIT / OPENING", b.placed[0].OrderType, b.placed[0].Condition)
	}
	if err := env.Ledger.UpdateStatus(b.placed[0].ClientOrderID, domain.OrderStatusExpired, decimal.Zero, nil, nil); err != nil {
		t.Fatal(err)
	}
	placed, err := PlacedToday(env)
	if err != nil {
		t.Fatal(err)
	}
	if placed.Long != 1 || placed.Symbols["7203"] != domain.SideBuy {
		t.Errorf("失効した寄指が枠に数えられていない: long=%d symbols=%v", placed.Long, placed.Symbols)
	}
	if unfilled, err := UnfilledOpening(env); err != nil || len(unfilled) != 1 || unfilled[0] != "7203" {
		t.Errorf("UnfilledOpening = %v, %v", unfilled, err)
	}

	if err := env.Ledger.UpdateStatus(b.placed[0].ClientOrderID, domain.OrderStatusRejected, decimal.Zero, nil, nil); err != nil {
		t.Fatal(err)
	}
	if placed, _ = PlacedToday(env); placed.Long != 0 {
		t.Errorf("拒否された寄指は埋め直す: long=%d", placed.Long)
	}
}
