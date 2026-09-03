package execute

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// stubBroker は発注の結果だけを差し替えられるブローカー。
type stubBroker struct {
	broker.Broker // 使わないメソッドは呼ばれたら panic する
	history       []domain.Order
	placeErr      error
	placed        []domain.OrderRequest
}

func (s *stubBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	return s.history, nil
}

func (s *stubBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	s.placed = append(s.placed, req)
	if s.placeErr != nil {
		return nil, s.placeErr
	}
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, Status: domain.OrderStatusSubmitted}, nil
}

// inspectingBroker は Place の最中に呼び出し側の状態を覗く。
type inspectingBroker struct {
	broker.Broker
	onPlace func()
	calls   int
}

func (i *inspectingBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	i.calls++
	i.onPlace()
	return &domain.OrderAck{ClientOrderID: req.ClientOrderID, Status: domain.OrderStatusSubmitted}, nil
}

// historyErrBroker は約定履歴の照会に失敗する。
type historyErrBroker struct{ stubBroker }

func (h *historyErrBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	return nil, errors.New("接続できません")
}

func newLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	led, err := ledger.OpenLedger(filepath.Join(t.TempDir(), "accum.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led
}

func newRequest(t *testing.T, id, symbol string, qty int64) domain.OrderRequest {
	t.Helper()
	price := decimal.NewFromInt(1000)
	req, err := domain.NewOrderRequest(id, symbol, domain.SideBuy, domain.OrderTypeLimit,
		decimal.NewFromInt(qty), &price, domain.TaxAccountSpecific, "テスト", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func dec(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// --- UnrecordedFills ---------------------------------------------------

func TestUnrecordedFillsDetectsLostLedger(t *testing.T) {
	led := newLedger(t)
	now := time.Date(2026, 9, 15, 5, 0, 0, 0, time.UTC)
	price := dec(1000)

	b := &stubBroker{history: []domain.Order{{
		ClientOrderID:  "台帳に無いID",
		Symbol:         "1306",
		Side:           domain.SideBuy,
		Quantity:       dec(100),
		FilledQuantity: dec(100),
		AvgFillPrice:   &price,
		Status:         domain.OrderStatusFilled,
	}}}

	found, err := UnrecordedFills(led, b, []string{"1306"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("台帳に無い約定を見つけるべき: %v", found)
	}
	if got := found["1306"]; !got.Equal(dec(100000)) {
		t.Errorf("約定額 = %s, want 100000", got)
	}
}

func TestUnrecordedFillsIgnoresKnownOrders(t *testing.T) {
	led := newLedger(t)
	now := time.Date(2026, 9, 15, 5, 0, 0, 0, time.UTC)
	req := newRequest(t, "既知のID", "1306", 100)
	month := "2026-09-01"
	amount := dec(100000)
	market := domain.MarketJP
	if err := led.Record(req, string(domain.OrderStatusFilled), nil, &month, &amount, &market); err != nil {
		t.Fatal(err)
	}

	price := dec(1000)
	b := &stubBroker{history: []domain.Order{{
		ClientOrderID:  "既知のID",
		Symbol:         "1306",
		Side:           domain.SideBuy,
		Quantity:       dec(100),
		FilledQuantity: dec(100),
		AvgFillPrice:   &price,
	}}}

	found, err := UnrecordedFills(led, b, []string{"1306"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("台帳にある注文は数えない: %v", found)
	}
}

func TestUnrecordedFillsIgnoresSellsAndUnfilled(t *testing.T) {
	led := newLedger(t)
	now := time.Date(2026, 9, 15, 5, 0, 0, 0, time.UTC)
	price := dec(1000)

	b := &stubBroker{history: []domain.Order{
		{ClientOrderID: "売り", Symbol: "1306", Side: domain.SideSell,
			FilledQuantity: dec(100), AvgFillPrice: &price},
		{ClientOrderID: "未約定", Symbol: "1306", Side: domain.SideBuy,
			FilledQuantity: decimal.Zero, AvgFillPrice: &price},
		{ClientOrderID: "対象外銘柄", Symbol: "9999", Side: domain.SideBuy,
			FilledQuantity: dec(100), AvgFillPrice: &price},
	}}

	found, err := UnrecordedFills(led, b, []string{"1306"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("売り・未約定・対象外は数えない: %v", found)
	}
}

func TestUnrecordedFillsPropagatesBrokerError(t *testing.T) {
	// 照会できないなら二重買付を否定できない。エラーを握り潰してはいけない。
	led := newLedger(t)
	b := &historyErrBroker{}
	if _, err := UnrecordedFills(led, b, []string{"1306"}, clock.NowUTC()); err == nil {
		t.Error("照会失敗はエラーとして返すべき")
	}
}

// --- placeRecorded -----------------------------------------------------

func TestPlaceRecordedWritesPendingBeforeSending(t *testing.T) {
	led := newLedger(t)
	req := newRequest(t, "注文1", "1306", 100)

	// 送信の最中に台帳を覗く。ここで記録済みでなければ、応答が返らなかった
	// ときに次回の run が同じ注文を送り直してしまう。
	var recordedWhenSent bool
	b := &inspectingBroker{
		onPlace: func() { recordedWhenSent = led.WasPlaced(req.ClientOrderID) },
	}

	if led.WasPlaced(req.ClientOrderID) {
		t.Fatal("最初は記録されていないはず")
	}

	ack, err := placeRecorded(b, led, req, "2026-09-01", dec(100000), domain.MarketJP)
	if err != nil {
		t.Fatal(err)
	}
	if !recordedWhenSent {
		t.Error("発注を送る前に台帳へ記録されているべき（応答不達で二重発注になる）")
	}
	if ack.Status != domain.OrderStatusSubmitted {
		t.Errorf("受理状態が反映されていない: %s", ack.Status)
	}
	if b.calls != 1 {
		t.Errorf("発注は1回のはず: %d", b.calls)
	}
}

func TestPlaceRecordedKeepsPendingOnUnconfirmed(t *testing.T) {
	// 通信断では届いたか分からない。送信中のまま残して再送を止める。
	led := newLedger(t)
	req := newRequest(t, "注文2", "1306", 100)
	b := &stubBroker{placeErr: &broker.BrokerError{Message: "タイムアウト"}}

	_, err := placeRecorded(b, led, req, "2026-09-01", dec(100000), domain.MarketJP)

	var unconfirmed *ErrUnconfirmedOrder
	if !errors.As(err, &unconfirmed) {
		t.Fatalf("ErrUnconfirmedOrder を返すべき: %v", err)
	}
	if !led.WasPlaced(req.ClientOrderID) {
		t.Error("送信中の記録が残り、次回の再送が止まるべき")
	}
}

func TestPlaceRecordedMarksRejected(t *testing.T) {
	// 明確な拒否は届いた上での拒否。次回の差額で埋め直せるよう REJECTED にする。
	led := newLedger(t)
	req := newRequest(t, "注文3", "1306", 100)
	b := &stubBroker{placeErr: &broker.OrderRejectedError{Message: "値幅制限"}}

	_, err := placeRecorded(b, led, req, "2026-09-01", dec(100000), domain.MarketJP)
	if err == nil {
		t.Fatal("拒否はエラーとして返すべき")
	}
	var unconfirmed *ErrUnconfirmedOrder
	if errors.As(err, &unconfirmed) {
		t.Error("明確な拒否を「確認できず」に混ぜてはいけない")
	}
	// REJECTED は WasPlaced の対象外なので、次回もう一度出せる。
	if led.WasPlaced(req.ClientOrderID) {
		t.Error("拒否された注文は再送を妨げないべき")
	}
}

// --- 発注時間帯 --------------------------------------------------------

func TestWindowState(t *testing.T) {
	cfg := &accumcfg.AccumConfig{
		Tactics: []accumcfg.TacticEntry{{
			ID: "A", Tactic: "constant", Symbols: []string{"1306.T"},
			MonthlyBudget: dec(10000), Window: window.Default(),
		}},
	}

	// 2026-09-03 は木曜。14:30 JST は時間内。
	inWindow := time.Date(2026, 9, 3, 14, 30, 0, 0, clock.Tokyo)
	if allowed, _ := WindowState(cfg, inWindow); !allowed {
		t.Error("時間内なら許すべき")
	}

	outWindow := time.Date(2026, 9, 3, 10, 0, 0, 0, clock.Tokyo)
	allowed, desc := WindowState(cfg, outWindow)
	if allowed {
		t.Error("時間外は許さないべき")
	}
	if desc != "14:00〜15:00 JST" {
		t.Errorf("時間帯の説明 = %q", desc)
	}
}

func TestWindowStateIgnoresDisabledTactics(t *testing.T) {
	disabled := false
	cfg := &accumcfg.AccumConfig{
		Tactics: []accumcfg.TacticEntry{{
			ID: "止めた", Tactic: "constant", Symbols: []string{"1306.T"},
			MonthlyBudget: dec(10000), Window: window.Unrestricted(), Enabled: &disabled,
		}},
	}
	// 無効な戦略が「制限なし」でも、有効な戦略が無ければ発注に進まない。
	if allowed, _ := WindowState(cfg, clock.NowUTC()); allowed {
		t.Error("無効な戦略の時間帯を見てはいけない")
	}
}

// --- 古い足 ------------------------------------------------------------

func TestIsStale(t *testing.T) {
	bars := []domain.Bar{{Date: "2026-09-01"}}

	cases := []struct {
		name      string
		today     string
		maxDays   int
		wantStale bool
		wantAge   int
	}{
		{"当日", "2026-09-01", 6, false, 0},
		{"6日前は許す", "2026-09-07", 6, false, 6},
		{"7日前は古い", "2026-09-08", 6, true, 7},
		{"上限0なら判定しない", "2026-12-01", 0, false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stale, age := isStale(bars, tc.today, tc.maxDays)
			if stale != tc.wantStale || age != tc.wantAge {
				t.Errorf("isStale = (%v, %d), want (%v, %d)", stale, age, tc.wantStale, tc.wantAge)
			}
		})
	}
}

func TestBrokerSymbol(t *testing.T) {
	cases := map[string]string{
		"1306.T": "1306",
		"^GSPC":  "GSPC",
		"VOO":    "VOO",
	}
	for in, want := range cases {
		if got := BrokerSymbol(in); got != want {
			t.Errorf("BrokerSymbol(%q) = %q, want %q", in, got, want)
		}
	}
}
