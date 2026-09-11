package execute

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
)

// historyFill は注文一覧から返る体裁の買い約定（立花は client_order_id を持たないので
// ClientOrderID にも注文番号が入る）。
func historyFill(symbol, brokerID string, qty, price int64) domain.Order {
	p := dec(price)
	id := brokerID
	return domain.Order{
		ClientOrderID:  id,
		BrokerOrderID:  &id,
		Symbol:         symbol,
		Side:           domain.SideBuy,
		OrderType:      domain.OrderTypeOther,
		Quantity:       dec(qty),
		FilledQuantity: dec(qty),
		AvgFillPrice:   &p,
		Status:         domain.OrderStatusFilled,
		Trade:          domain.TradeTypeCash,
	}
}

// --apply が無ければ台帳を書かない。
func TestImportFillsDryRun(t *testing.T) {
	led := newLedger(t)
	b := &stubBroker{history: []domain.Order{historyFill("563A", "11014725/20260911", 1000, 1000)}}

	fills, err := ImportFills(led, b, &logging.Logger{}, ImportFillsOptions{Symbols: []string{"563A"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || !fills[0].Amount.Equal(dec(1000000)) {
		t.Fatalf("約定額 = %v, want 1000000", fills)
	}
	orders, err := led.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 0 {
		t.Errorf("dry-run で台帳に書いている: %v", orders)
	}
}

// --apply で台帳に入り、当月の投下額として数えられる。2 度叩いても重複しない。
func TestImportFillsApplyIsIdempotent(t *testing.T) {
	led := newLedger(t)
	b := &stubBroker{history: []domain.Order{historyFill("563A", "11014725/20260911", 1000, 1000)}}
	opts := ImportFillsOptions{Symbols: []string{"563A"}, Apply: true}

	if _, err := ImportFills(led, b, &logging.Logger{}, opts); err != nil {
		t.Fatal(err)
	}
	// 2 回目は「台帳に無い約定」が無くなるので 0 件
	fills, err := ImportFills(led, b, &logging.Logger{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 0 {
		t.Errorf("2 度目も取り込もうとしている: %v", fills)
	}
	orders, err := led.Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("行が %d 件（1 件のはず）", len(orders))
	}
	got := orders[0]
	if got.ClientOrderID != "11014725/20260911" {
		t.Errorf("client_order_id = %s（注文番号を入れるはず）", got.ClientOrderID)
	}
	if got.Status != string(domain.OrderStatusFilled) {
		t.Errorf("状態 = %s, want FILLED", got.Status)
	}
	if got.Amount == nil || !got.Amount.Equal(dec(1000000)) {
		t.Errorf("投下額 = %v, want 1000000", got.Amount)
	}
	if !got.FilledQuantity.Equal(dec(1000)) {
		t.Errorf("約定数量 = %v, want 1000", got.FilledQuantity)
	}
}

// --order で注文番号を選べる。同じ銘柄に検証の注文と本当の買いが混じる日に使う。
func TestImportFillsFiltersByOrderNumber(t *testing.T) {
	led := newLedger(t)
	b := &stubBroker{history: []domain.Order{
		historyFill("563A", "11010971/20260911", 1, 998),     // 検証の注文
		historyFill("563A", "11014725/20260911", 1000, 1000), // 手で買ったぶん
	}}

	// 営業日を付けずに番号だけでも指定できる
	fills, err := ImportFills(led, b, &logging.Logger{}, ImportFillsOptions{
		Symbols: []string{"563A"}, Orders: []string{"11014725"}, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 1 || !fills[0].Quantity.Equal(dec(1000)) {
		t.Fatalf("指定した 1 件だけ取り込むべき: %v", fills)
	}
}

// 売りと未約定は投下額ではないので入れない。
func TestImportFillsSkipsSellsAndUnfilled(t *testing.T) {
	led := newLedger(t)
	sell := historyFill("563A", "11011801/20260911", 1, 997)
	sell.Side = domain.SideSell
	unfilled := historyFill("563A", "11011704/20260911", 1, 998)
	unfilled.FilledQuantity = dec(0)
	b := &stubBroker{history: []domain.Order{sell, unfilled}}

	fills, err := ImportFills(led, b, &logging.Logger{}, ImportFillsOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fills) != 0 {
		t.Errorf("売り・未約定を取り込んでいる: %v", fills)
	}
}
