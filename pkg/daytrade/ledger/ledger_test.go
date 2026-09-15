package ledger

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func openTest(t *testing.T) *Ledger {
	t.Helper()
	led, err := Open(filepath.Join(t.TempDir(), "daytrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	return led
}

var day = time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

func request(id, symbol string, side domain.Side, qty int64, trade domain.TradeType) domain.OrderRequest {
	return domain.OrderRequest{
		ClientOrderID: id, Symbol: symbol, Side: side,
		OrderType: domain.OrderTypeMarket, Quantity: decimal.NewFromInt(qty),
		Trade: trade, Reason: "test",
	}
}

func TestRecordAndRead(t *testing.T) {
	led := openTest(t)
	price := decimal.NewFromInt(1000)
	if err := led.Record(request("a", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), &price, nil); err != nil {
		t.Fatal(err)
	}
	orders, err := led.OrdersOn(day, nil)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders = %d, %v", len(orders), err)
	}
	o := orders[0]
	if o.Symbol != "7203" || !o.Quantity.Equal(decimal.NewFromInt(100)) || o.Price == nil {
		t.Errorf("読み戻しが一致しない: %+v", o)
	}
	if !o.IsEntry() || o.Leg() != "long" {
		t.Errorf("現物の買いは建てる側・ロングのはず: entry=%v leg=%s", o.IsEntry(), o.Leg())
	}
}

func TestLegAndEntryExit(t *testing.T) {
	cases := []struct {
		side      domain.Side
		trade     domain.TradeType
		wantEntry bool
		wantLeg   string
	}{
		{domain.SideBuy, domain.TradeTypeCash, true, "long"},
		{domain.SideSell, domain.TradeTypeCash, false, "long"},        // 現物の売り = ロングの手仕舞い
		{domain.SideBuy, domain.TradeTypeMarginOpen, true, "long"},    // 信用買い
		{domain.SideSell, domain.TradeTypeMarginOpen, true, "short"},  // 売建
		{domain.SideBuy, domain.TradeTypeMarginClose, false, "short"}, // 返済買い
		{domain.SideSell, domain.TradeTypeMarginClose, false, "long"}, // 返済売り
	}
	for _, c := range cases {
		o := Order{Side: c.side, Trade: c.trade}
		if o.IsEntry() != c.wantEntry || o.Leg() != c.wantLeg {
			t.Errorf("%s/%s: entry=%v leg=%s, want %v/%s",
				c.side, c.trade, o.IsEntry(), o.Leg(), c.wantEntry, c.wantLeg)
		}
	}
}

func TestWasPlacedIgnoresDryRunAndDead(t *testing.T) {
	led := openTest(t)
	req := request("dry", "7203", domain.SideBuy, 100, domain.TradeTypeCash)
	_ = led.Record(req, day, DryRunStatus, nil, nil)
	if placed, err := led.WasPlaced("dry"); err != nil || placed {
		t.Errorf("dry-run を発注済みと数えている: %v %v", placed, err)
	}
	if placed, err := led.WasPlaced("none"); err != nil || placed {
		t.Errorf("記録の無い注文: %v %v", placed, err)
	}
	req2 := request("live", "9984", domain.SideBuy, 100, domain.TradeTypeCash)
	_ = led.Record(req2, day, string(domain.OrderStatusSubmitted), nil, nil)
	if placed, err := led.WasPlaced("live"); err != nil || !placed {
		t.Errorf("本発注を発注済みと数えていない: %v %v", placed, err)
	}
	// 拒否で終わった注文は「送り直してよい」
	_ = led.UpdateStatus("live", domain.OrderStatusRejected, decimal.Zero, nil, nil)
	if placed, err := led.WasPlaced("live"); err != nil || placed {
		t.Errorf("拒否された注文を発注済みと数えている: %v %v", placed, err)
	}
	if n := led.DeadCount(day, "9984", domain.SideBuy); n != 1 {
		t.Errorf("DeadCount = %d, want 1", n)
	}
}

// 台帳を読めないときに「未発注」と答えると、既に送った注文をもう一度送る（二重発注）。
func TestWasPlacedFailsClosedOnDBError(t *testing.T) {
	led := openTest(t)
	_ = led.Record(request("live", "9984", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), nil, nil)
	if err := led.Close(); err != nil {
		t.Fatal(err)
	}
	placed, err := led.WasPlaced("live")
	if err == nil {
		t.Fatalf("閉じた台帳でエラーにならない（placed=%v）", placed)
	}
	if placed {
		t.Error("エラーなのに発注済みと答えた")
	}
}

// 執行の滑りは「送る直前の時価」と約定単価の差でしか測れない。
// 判断時の気配（Price）も残したまま、両方が 1 行に並ぶこと。
func TestSetRefKeepsDecisionAndExecutionPrices(t *testing.T) {
	led := openTest(t)
	quote := decimal.NewFromInt(1000)
	if err := led.Record(request("a", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), &quote, nil); err != nil {
		t.Fatal(err)
	}
	ref := Ref{
		Price: decimal.NewFromInt(1020), Bid: decimal.NewFromInt(1019), Ask: decimal.NewFromInt(1021),
		At: time.Date(2026, 9, 16, 0, 4, 0, 0, time.UTC),
	}
	if err := led.SetRef("a", ref); err != nil {
		t.Fatal(err)
	}
	fill := decimal.NewFromInt(1025)
	if err := led.UpdateStatus("a", domain.OrderStatusFilled, decimal.NewFromInt(100), &fill, nil); err != nil {
		t.Fatal(err)
	}
	o, ok, err := led.Get("a")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if o.Price == nil || !o.Price.Equal(quote) {
		t.Errorf("判断時の気配が消えた: %v", o.Price)
	}
	if o.RefPrice == nil || !o.RefPrice.Equal(ref.Price) {
		t.Errorf("執行時の時価 = %v, want 1020", o.RefPrice)
	}
	if o.RefBid == nil || o.RefAsk == nil || !o.RefBid.Equal(ref.Bid) || !o.RefAsk.Equal(ref.Ask) {
		t.Errorf("最良気配が残っていない: %v / %v", o.RefBid, o.RefAsk)
	}
	if o.RefAt == nil || *o.RefAt != "2026-09-16T00:04:00Z" {
		t.Errorf("時価の時刻 = %v", o.RefAt)
	}
	if o.AvgFillPrice == nil || !o.AvgFillPrice.Equal(fill) {
		t.Errorf("約定単価が消えた: %v", o.AvgFillPrice)
	}
}

// 時価を控えていない注文（この機能より前の行）は null のまま読める。
func TestRefAbsentIsNil(t *testing.T) {
	led := openTest(t)
	if err := led.Record(request("a", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), nil, nil); err != nil {
		t.Fatal(err)
	}
	o, _, err := led.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if o.RefPrice != nil || o.RefBid != nil || o.RefAsk != nil || o.RefAt != nil {
		t.Errorf("記録していない時価が入っている: %+v", o)
	}
}

// 手仕舞いが 2 本（引けの一部約定 + 翌寄りの返済）なら、基準の時価も約定数量で加重する。
func TestExitAvgRefWeightsByFilledQuantity(t *testing.T) {
	led := openTest(t)
	exits := []Order{}
	for _, e := range []struct {
		id       string
		qty, ref int64
	}{{"e1", 200, 1000}, {"e2", 100, 1300}} {
		refPrice := decimal.NewFromInt(e.ref)
		if err := led.Record(request(e.id, "7203", domain.SideSell, e.qty, domain.TradeTypeCash), day,
			string(domain.OrderStatusSubmitted), nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := led.SetRef(e.id, Ref{Price: refPrice, At: day}); err != nil {
			t.Fatal(err)
		}
		if err := led.UpdateStatus(e.id, domain.OrderStatusFilled, decimal.NewFromInt(e.qty), &refPrice, nil); err != nil {
			t.Fatal(err)
		}
		o, _, err := led.Get(e.id)
		if err != nil {
			t.Fatal(err)
		}
		exits = append(exits, o)
	}
	avg, ok := ExitAvgRef(exits)
	if !ok || !avg.Equal(decimal.NewFromInt(1100)) {
		t.Errorf("加重平均 = %v (ok=%v), want 1100（200 株 × 1000 + 100 株 × 1300）", avg, ok)
	}
}

// fillOrder は注文を記録して約定させる（数量と単価）。
func fillOrder(t *testing.T, led *Ledger, id, symbol string, side domain.Side, trade domain.TradeType, d time.Time, qty, price int64) {
	t.Helper()
	p := decimal.NewFromInt(price)
	if err := led.Record(request(id, symbol, side, qty, trade), d, string(domain.OrderStatusSubmitted), &p, nil); err != nil {
		t.Fatal(err)
	}
	if err := led.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(qty), &p, nil); err != nil {
		t.Fatal(err)
	}
}

// 引けの手仕舞いが 200 株だけ約定し、残り 100 株を翌寄りの持ち越し返済で手仕舞った。
// 手仕舞いが 2 本になっても 300 株ぶんの損益になる（最後の 1 本だけを見ない）。
func TestRealizedPnLAggregatesMultipleExits(t *testing.T) {
	led := openTest(t)
	fillOrder(t, led, "b", "7203", domain.SideBuy, domain.TradeTypeMarginOpen, day, 300, 1000)
	fillOrder(t, led, "s1", "7203", domain.SideSell, domain.TradeTypeMarginClose, day, 200, 1010)

	// 返済が 200 株だけの間は未確定（残り 100 株の損益が決まっていない）
	pnl, err := led.RealizedPnL([]time.Time{day}, "long")
	if err != nil {
		t.Fatal(err)
	}
	if v := pnl[day.Format(dayLayout)]; v != nil {
		t.Errorf("持ち越しの残りがあるのに確定している: %v", *v)
	}

	// 翌寄りの返済（建てた日の下に記録される）で 100 株を 990 円
	fillOrder(t, led, "s2", "7203", domain.SideSell, domain.TradeTypeMarginClose, day, 100, 990)
	pnl, _ = led.RealizedPnL([]time.Time{day}, "long")
	// 200 × (+10) + 100 × (−10) = +1,000 円
	if v := pnl[day.Format(dayLayout)]; v == nil || *v != 1000 {
		t.Errorf("300 株ぶんの損益 = %v, want 1000", v)
	}

	entry, _, _ := led.Get("b")
	s1, _, _ := led.Get("s1")
	s2, _, _ := led.Get("s2")
	avg, filled, ok := ExitAvgPrice([]Order{s1, s2})
	// (200 × 1010 + 100 × 990) ÷ 300 = 1003.33…
	if !ok || !filled.Equal(decimal.NewFromInt(300)) || avg.Round(2).String() != "1003.33" {
		t.Errorf("加重平均 = %s × %s（ok=%v）", avg, filled, ok)
	}
	if v, ok := RealizedOf(entry, []Order{s1, s2}); !ok || v != 1000 {
		t.Errorf("RealizedOf = %v（ok=%v）", v, ok)
	}
}

// 検証の注文だけの日々は「建てていない」。数えると損益 0 のまま資金が半分に縮む。
func TestRecentPnLIgnoresVerifyOnlyHistory(t *testing.T) {
	led := openTest(t)
	led.Verify = true
	days := []time.Time{day, day.AddDate(0, 0, 1)}
	for i, d := range days {
		fillOrder(t, led, "vb"+string(rune('0'+i)), "7203", domain.SideBuy, domain.TradeTypeCash, d, 100, 1000)
		fillOrder(t, led, "vs"+string(rune('0'+i)), "7203", domain.SideSell, domain.TradeTypeCash, d, 100, 1000)
	}
	total, _, err := led.RecentPnL(days, "long")
	if err != nil {
		t.Fatal(err)
	}
	if total != nil {
		t.Errorf("検証の注文しか無いのにゲートが効く: %v", *total)
	}

	// 本番の往復が 1 本あれば数える
	led.Verify = false
	fillOrder(t, led, "b", "9984", domain.SideBuy, domain.TradeTypeCash, day, 100, 1000)
	fillOrder(t, led, "s", "9984", domain.SideSell, domain.TradeTypeCash, day, 100, 900)
	total, incomplete, err := led.RecentPnL(days, "long")
	if err != nil || total == nil || *total != -10000 || len(incomplete) != 0 {
		t.Errorf("本番の損益 = %v（incomplete %v, err %v）, want -10000", total, incomplete, err)
	}
}

func TestClearDryRun(t *testing.T) {
	led := openTest(t)
	_ = led.Record(request("d1", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day, DryRunStatus, nil, nil)
	_ = led.Record(request("l1", "9984", domain.SideBuy, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), nil, nil)
	n, err := led.ClearDryRun(day)
	if err != nil || n != 1 {
		t.Fatalf("ClearDryRun = %d, %v", n, err)
	}
	orders, _ := led.OrdersOn(day, nil)
	if len(orders) != 1 || orders[0].ClientOrderID != "l1" {
		t.Errorf("本発注まで消している: %+v", orders)
	}
}

func TestRealizedPnLLongAndShort(t *testing.T) {
	led := openTest(t)
	buy := decimal.NewFromInt(1000)
	sell := decimal.NewFromInt(1010)
	// ロング: 1000 で買い 1010 で売り × 100 株 = +1000 円
	_ = led.Record(request("b", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), &buy, nil)
	_ = led.UpdateStatus("b", domain.OrderStatusFilled, decimal.NewFromInt(100), &buy, nil)
	_ = led.Record(request("s", "7203", domain.SideSell, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), &sell, nil)
	_ = led.UpdateStatus("s", domain.OrderStatusFilled, decimal.NewFromInt(100), &sell, nil)

	pnl, err := led.RealizedPnL([]time.Time{day}, "long")
	if err != nil {
		t.Fatal(err)
	}
	got := pnl[day.Format("2006-01-02")]
	if got == nil || *got != 1000 {
		t.Fatalf("ロングの損益 = %v, want 1000", got)
	}
	// ショートの脚だけを見ると、その日は建てていないので 0
	pnl, _ = led.RealizedPnL([]time.Time{day}, "short")
	if v := pnl[day.Format("2006-01-02")]; v == nil || *v != 0 {
		t.Errorf("ショートの損益 = %v, want 0", v)
	}
}

func TestRealizedPnLIncompleteIsNil(t *testing.T) {
	// 建てて約定したのに手仕舞いの単価が無い日は nil（0 と混ぜると「負けた」と誤読する）
	led := openTest(t)
	buy := decimal.NewFromInt(1000)
	_ = led.Record(request("b", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), &buy, nil)
	_ = led.UpdateStatus("b", domain.OrderStatusFilled, decimal.NewFromInt(100), &buy, nil)
	pnl, _ := led.RealizedPnL([]time.Time{day}, "long")
	if v := pnl[day.Format("2006-01-02")]; v != nil {
		t.Errorf("未確定の日が nil でない: %v", *v)
	}
}

func TestRealizedPnLShortSide(t *testing.T) {
	led := openTest(t)
	open := decimal.NewFromInt(1100)
	closePrice := decimal.NewFromInt(1050)
	// 売建 1100 → 返済買い 1050 × 100 株 = +5000 円
	_ = led.Record(request("so", "7203", domain.SideSell, 100, domain.TradeTypeMarginOpen), day, string(domain.OrderStatusFilled), &open, nil)
	_ = led.UpdateStatus("so", domain.OrderStatusFilled, decimal.NewFromInt(100), &open, nil)
	_ = led.Record(request("sc", "7203", domain.SideBuy, 100, domain.TradeTypeMarginClose), day, string(domain.OrderStatusFilled), &closePrice, nil)
	_ = led.UpdateStatus("sc", domain.OrderStatusFilled, decimal.NewFromInt(100), &closePrice, nil)

	pnl, _ := led.RealizedPnL([]time.Time{day}, "short")
	if v := pnl[day.Format("2006-01-02")]; v == nil || *v != 5000 {
		t.Errorf("ショートの損益 = %v, want 5000", v)
	}
}

func TestEntriesAndExits(t *testing.T) {
	led := openTest(t)
	_ = led.Record(request("b", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), nil, nil)
	_ = led.Record(request("s", "7203", domain.SideSell, 100, domain.TradeTypeCash), day, string(domain.OrderStatusFilled), nil, nil)
	entries, _ := led.EntriesOn(day)
	exits, _ := led.ExitsOn(day)
	if len(entries) != 1 || len(exits) != 1 {
		t.Errorf("entries=%d exits=%d, want 1/1", len(entries), len(exits))
	}
}

func TestBackup(t *testing.T) {
	led := openTest(t)
	_ = led.Record(request("b", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day, DryRunStatus, nil, nil)
	dest := filepath.Join(t.TempDir(), "backup.db")
	if err := led.Backup(dest); err != nil {
		t.Fatal(err)
	}
	copied, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	orders, _ := copied.OrdersOn(day, nil)
	if len(orders) != 1 {
		t.Errorf("バックアップに注文が無い: %d", len(orders))
	}
}

func TestVerifyMarksOrders(t *testing.T) {
	led := openTest(t)
	price := decimal.NewFromInt(1000)
	// 通常の実行 → 印なし
	if err := led.Record(request("a", "7203", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), &price, nil); err != nil {
		t.Fatal(err)
	}
	// 実機検証の実行 → 印あり。env を変えずに切り分けられること
	led.Verify = true
	if err := led.Record(request("b", "9984", domain.SideBuy, 100, domain.TradeTypeCash), day,
		string(domain.OrderStatusSubmitted), &price, nil); err != nil {
		t.Fatal(err)
	}
	orders, err := led.OrdersOn(day, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, o := range orders {
		got[o.ClientOrderID] = o.Verify
	}
	if got["a"] {
		t.Error("通常の注文に検証の印が付いた")
	}
	if !got["b"] {
		t.Error("検証の注文に印が付いていない")
	}
}

func TestRealizedPnLSkipsVerify(t *testing.T) {
	led := openTest(t)
	fill := func(id string, price int64) {
		p := decimal.NewFromInt(price)
		if err := led.UpdateStatus(id, domain.OrderStatusFilled, decimal.NewFromInt(100), &p, nil); err != nil {
			t.Fatal(err)
		}
	}
	entry := decimal.NewFromInt(1000)
	// 検証で建てて手仕舞った 1 往復だけを台帳に置く
	led.Verify = true
	for id, side := range map[string]domain.Side{"v-buy": domain.SideBuy, "v-sell": domain.SideSell} {
		if err := led.Record(request(id, "7203", side, 100, domain.TradeTypeCash), day,
			string(domain.OrderStatusSubmitted), &entry, nil); err != nil {
			t.Fatal(err)
		}
	}
	fill("v-buy", 1000)
	fill("v-sell", 900) // 1 万円の損。ゲートに数えると当日の資金が縮む

	pnl, err := led.RealizedPnL([]time.Time{day}, "long")
	if err != nil {
		t.Fatal(err)
	}
	// 検証の往復しか無い日は「建てていない日」と同じ 0 になる（縮小の合図にしない）
	got := pnl[day.Format(dayLayout)]
	if got == nil {
		t.Fatalf("損益が nil（検証の注文を数えて未確定になっている）")
	}
	if *got != 0 {
		t.Errorf("検証の損益が資産曲線に入った: %v", *got)
	}
}
