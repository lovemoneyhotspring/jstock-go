package broker

import (
	"errors"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func dec(v string) decimal.Decimal { return decimal.RequireFromString(v) }

// 「行が無い」と「応答の形が分からない」は必ず別物として扱う。
// ここが崩れると、項目名の食い違いが「注文なし」「約定 0 株」として通る。
func TestRowsOfDistinguishesEmptyFromUnknown(t *testing.T) {
	// キーがある・中身が空 → 正常な「該当なし」
	rows, err := rowsOf(map[string]any{orderListKey: []any{}}, orderListKey, clmOrderList)
	if err != nil || len(rows) != 0 {
		t.Errorf("空配列は該当なしとして通すべき: rows=%v err=%v", rows, err)
	}

	// キーが null → 該当なし
	if _, err := rowsOf(map[string]any{orderListKey: nil}, orderListKey, clmOrderList); err != nil {
		t.Errorf("null は該当なしとして通すべき: %v", err)
	}

	// キーが無い → 形が分からない。必ずエラー
	_, err = rowsOf(map[string]any{"sResultCode": "0", "aSomethingElse": []any{}},
		orderListKey, clmOrderList)
	var unverified *ErrUnverifiedResponse
	if !errors.As(err, &unverified) {
		t.Fatalf("キーが無い応答はエラーにすべき: %v", err)
	}
	if len(unverified.Got) == 0 {
		t.Error("エラーに実際のキーが載っていません（実機で直す手掛かりになる）")
	}
}

func TestCheckResult(t *testing.T) {
	if err := checkResult(map[string]any{"sResultCode": "0"}, clmOrderList); err != nil {
		t.Errorf("正常応答が弾かれました: %v", err)
	}
	if err := checkResult(map[string]any{"sResultCode": "1", "sResultText": "だめ"}, clmOrderList); err == nil {
		t.Error("業務エラーが通ってしまいました")
	}
	// sResultCode 自体が無い＝電文が違う。成功とみなさない
	if err := checkResult(map[string]any{"foo": "bar"}, clmOrderList); err == nil {
		t.Error("sResultCode の無い応答が通ってしまいました")
	}
}

// 売買区分は 1＝売、3＝買。現渡(5)・現引(7)は扱わないのでエラー。
// 逆に覚えると注文の向きが反対になる。
func TestParseSide(t *testing.T) {
	if side, err := ParseSide("3"); err != nil || side != domain.SideBuy {
		t.Errorf("3 → %s (%v), want BUY", side, err)
	}
	if side, err := ParseSide("1"); err != nil || side != domain.SideSell {
		t.Errorf("1 → %s (%v), want SELL", side, err)
	}
	for _, code := range []string{"5", "7", "", "9"} {
		if _, err := ParseSide(code); err == nil {
			t.Errorf("%q が売買区分として通ってしまいました", code)
		}
	}
}

// 知らない状態コードは Unknown に落とす。Unknown は IsTerminal が false なので、
// 「終わった注文」と誤認して台帳から落とすことはない。
func TestParseStatus(t *testing.T) {
	cases := map[string]domain.OrderStatus{
		"1":  domain.OrderStatusSubmitted,
		"9":  domain.OrderStatusPartiallyFilled,
		"10": domain.OrderStatusFilled, // 全部約定
		"7":  domain.OrderStatusCancelled,
		"12": domain.OrderStatusExpired,
		"50": domain.OrderStatusPending,
	}
	for code, want := range cases {
		if got := ParseStatus(code); got != want {
			t.Errorf("%s → %s, want %s", code, got, want)
		}
	}
	got := ParseStatus("99")
	if got != domain.OrderStatusUnknown {
		t.Errorf("未知コード → %s, want UNKNOWN", got)
	}
	if got.IsTerminal() {
		t.Error("未知コードを終了扱いにすると、確定していない注文が台帳から落ちる")
	}
}

// 現金信用区分は現物へ黙って落とさない。落とすと信用建玉を現物として数える。
func TestParseTrade(t *testing.T) {
	cases := map[string]domain.TradeType{
		"0": domain.TradeTypeCash,
		"2": domain.TradeTypeMarginOpen,
		"6": domain.TradeTypeMarginOpen, // 一般信用の新規
		"4": domain.TradeTypeMarginClose,
		"8": domain.TradeTypeMarginClose,
	}
	for code, want := range cases {
		got, err := ParseTrade(code)
		if err != nil || got != want {
			t.Errorf("%s → %s (%v), want %s", code, got, err, want)
		}
	}
	if _, err := ParseTrade("9"); err == nil {
		t.Error("未知の現金信用区分が通ってしまいました")
	}
}

// 発注に送るコードも取り違えない（一般=3、NISA=6。2 や 3 ではない）。
func TestOrderCodes(t *testing.T) {
	if got := taxCodeOf(domain.TaxAccountGeneral); got != "3" {
		t.Errorf("一般口座 = %s, want 3", got)
	}
	if got := taxCodeOf(domain.TaxAccountNISA); got != "6" {
		t.Errorf("NISA = %s, want 6", got)
	}
	if got := taxCodeOf(domain.TaxAccountSpecific); got != "1" {
		t.Errorf("特定口座 = %s, want 1", got)
	}
	if _, err := tradeCodeOf(domain.TradeType("SOMETHING_NEW")); err == nil {
		t.Error("未知の取引種別が現物として通ってしまいました")
	}
}

// 注文一覧の行を読む。項目名は電文ごとに違うので、一覧の名前で引けること。
func TestToOrderFromList(t *testing.T) {
	row := map[string]any{
		fieldListIssue:       "7203",
		fieldSideKubun:       "1", // 売
		fieldTradeKubun:      "2", // 信用新規
		fieldListQty:         "300",
		fieldListFilledQty:   "100",
		fieldListFillPrice:   "2500.5",
		fieldOrderStatusCode: "9", // 一部約定
		fieldPriceKubun:      "2", // 指値
		fieldOrderPrice:      "2490",
	}
	order, err := toOrder(row, listFields, "12345", "20260903", "")
	if err != nil {
		t.Fatalf("注文として読めません: %v", err)
	}
	if *order.BrokerOrderID != "12345/20260903" {
		t.Errorf("broker_order_id = %s", *order.BrokerOrderID)
	}
	if order.Side != domain.SideSell || order.Trade != domain.TradeTypeMarginOpen {
		t.Errorf("売買/取引種別 = %s / %s", order.Side, order.Trade)
	}
	if !order.Quantity.Equal(dec("300")) || !order.FilledQuantity.Equal(dec("100")) {
		t.Errorf("数量 = %s / %s", order.Quantity, order.FilledQuantity)
	}
	if order.Status != domain.OrderStatusPartiallyFilled {
		t.Errorf("状態 = %s", order.Status)
	}
	if order.OrderType != domain.OrderTypeLimit || order.LimitPrice == nil ||
		!order.LimitPrice.Equal(dec("2490")) {
		t.Errorf("注文種別/指値 = %s / %v", order.OrderType, order.LimitPrice)
	}
	if order.AvgFillPrice == nil || !order.AvgFillPrice.Equal(dec("2500.5")) {
		t.Errorf("約定単価 = %v", order.AvgFillPrice)
	}
}

// 単品照会は接頭辞の無い項目名。一覧の名前で読むと約定数量が 0 になる。
func TestToOrderFromDetailUsesOwnFieldNames(t *testing.T) {
	row := map[string]any{
		fieldDetailIssue:     "7203",
		fieldSideKubun:       "3",
		fieldTradeKubun:      "0",
		fieldListQty:         "100",
		fieldDetailFilledQty: "100",
		fieldDetailFillPrice: "2500",
		fieldOrderStatusCode: "10",
		fieldPriceKubun:      "1", // 成行
	}
	order, err := toOrder(row, detailFields, "12345", "20260903", "my-id")
	if err != nil {
		t.Fatalf("注文として読めません: %v", err)
	}
	if !order.FilledQuantity.Equal(dec("100")) {
		t.Errorf("約定数量 = %s, want 100（単品照会の項目名で読めていない）", order.FilledQuantity)
	}
	if order.ClientOrderID != "my-id" {
		t.Errorf("client_order_id = %s", order.ClientOrderID)
	}
	if order.OrderType != domain.OrderTypeMarket || order.LimitPrice != nil {
		t.Errorf("成行なのに指値が入っています: %s / %v", order.OrderType, order.LimitPrice)
	}
}

// 逆指値の項目を読む。一覧は sOrder* の接頭辞付き、トリガータイプ 0 が未発火。
func TestToOrderReadsStop(t *testing.T) {
	row := map[string]any{
		fieldListIssue:             "7203",
		fieldSideKubun:             "1", // 売
		fieldTradeKubun:            "4", // 信用返済
		fieldListQty:               "100",
		fieldListFilledQty:         "0",
		fieldOrderStatusCode:       "16", // 逆指注文（未約定）
		fieldPriceKubun:            "1",
		"sOrderGyakusasiOrderType": "1",
		"sOrderGyakusasiZyouken":   "2400",
		"sOrderGyakusasiKubun":     "1",
		"sOrderGyakusasiPrice":     "2380",
		"sOrderTriggerType":        "0",
	}
	order, err := toOrder(row, listFields, "12345", "20260903", "")
	if err != nil {
		t.Fatalf("注文として読めません: %v", err)
	}
	if order.Stop == nil || !order.Stop.Trigger.Equal(dec("2400")) ||
		order.Stop.Price == nil || !order.Stop.Price.Equal(dec("2380")) {
		t.Fatalf("逆指値が読めていない: %+v", order.Stop)
	}
	if order.StopTriggered {
		t.Error("未発火なのに発火済みになっている")
	}
	if !order.Status.IsOpen() {
		t.Errorf("逆指注文（未約定）が板に残っている扱いでない: %s", order.Status)
	}

	// 発火後の成行（値段区分 0）
	row["sOrderGyakusasiKubun"] = "0"
	row["sOrderGyakusasiPrice"] = "0"
	row["sOrderTriggerType"] = "1"
	order, err = toOrder(row, listFields, "12345", "20260903", "")
	if err != nil {
		t.Fatal(err)
	}
	if order.Stop == nil || order.Stop.Price != nil || !order.StopTriggered {
		t.Errorf("発火済み・成行の逆指値が読めていない: %+v / %v", order.Stop, order.StopTriggered)
	}

	// 逆指値の無い行には付けない
	delete(row, "sOrderGyakusasiOrderType")
	order, err = toOrder(row, listFields, "12345", "20260903", "")
	if err != nil {
		t.Fatal(err)
	}
	if order.Stop != nil {
		t.Errorf("逆指値の無い注文に条件が付いている: %+v", order.Stop)
	}
}

// 解釈できない売買区分の行は落とす（買いとして通さない）。
func TestToOrderRejectsUnknownSide(t *testing.T) {
	row := map[string]any{fieldSideKubun: "5", fieldTradeKubun: "0"} // 現渡
	if _, err := toOrder(row, listFields, "1", "20260903", ""); err == nil {
		t.Error("現渡の行が注文として通ってしまいました")
	}
}

func TestSplitBrokerOrderID(t *testing.T) {
	number, day, ok := splitBrokerOrderID("12345/20260903")
	if !ok || number != "12345" || day != "20260903" {
		t.Errorf("分解 = %s / %s (%v)", number, day, ok)
	}
	for _, bad := range []string{"", "12345", "/20260903", "12345/"} {
		if _, _, ok := splitBrokerOrderID(bad); ok {
			t.Errorf("%q を受け入れてしまいました", bad)
		}
	}
}
