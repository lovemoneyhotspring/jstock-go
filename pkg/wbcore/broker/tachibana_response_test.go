package broker

import (
	"errors"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 「行が無い」と「応答の形が分からない」は必ず別物として扱う。
// ここが崩れると、項目名の食い違いが「注文なし」「約定 0 株」として通る。
func TestRowsOfDistinguishesEmptyFromUnknown(t *testing.T) {
	// キーがある・中身が空 → 正常な「該当なし」
	rows, err := rowsOf(map[string]any{"aCLMOrderList": []any{}}, "aCLMOrderList", "CLMOrderList")
	if err != nil || len(rows) != 0 {
		t.Errorf("空配列は該当なしとして通すべき: rows=%v err=%v", rows, err)
	}

	// キーが null → 該当なし
	if _, err := rowsOf(map[string]any{"aCLMOrderList": nil}, "aCLMOrderList", "CLMOrderList"); err != nil {
		t.Errorf("null は該当なしとして通すべき: %v", err)
	}

	// キーが無い → 形が分からない。必ずエラー
	_, err = rowsOf(map[string]any{"sResultCode": "0", "aSomethingElse": []any{}},
		"aCLMOrderList", "CLMOrderList")
	var unverified *ErrUnverifiedResponse
	if !errors.As(err, &unverified) {
		t.Fatalf("キーが無い応答はエラーにすべき: %v", err)
	}
	// 実機で直す手掛かりとして、返ってきたキーを載せる
	if len(unverified.Got) == 0 {
		t.Error("エラーに実際のキーが載っていません")
	}
}

func TestCheckResult(t *testing.T) {
	if err := checkResult(map[string]any{"sResultCode": "0"}, "CLMOrderList"); err != nil {
		t.Errorf("正常応答が弾かれました: %v", err)
	}
	if err := checkResult(map[string]any{"sResultCode": "1", "sResultText": "だめ"}, "CLMOrderList"); err == nil {
		t.Error("業務エラーが通ってしまいました")
	}
	// sResultCode 自体が無い＝電文が違う。成功とみなさない
	if err := checkResult(map[string]any{"foo": "bar"}, "CLMOrderList"); err == nil {
		t.Error("sResultCode の無い応答が通ってしまいました")
	}
}

// 知らない状態コードは Unknown に落とす。Unknown は IsTerminal が false なので、
// 「終わった注文」と誤認して台帳から落とすことはない。
func TestParseTachibanaOrderStatusUnknownIsOpen(t *testing.T) {
	if got := ParseTachibanaOrderStatus("4"); got != domain.OrderStatusFilled {
		t.Errorf("4 → %s, want FILLED", got)
	}
	got := ParseTachibanaOrderStatus("99")
	if got != domain.OrderStatusUnknown {
		t.Errorf("未知コード → %s, want UNKNOWN", got)
	}
	if got.IsTerminal() {
		t.Error("未知コードを終了扱いにすると、確定していない注文が台帳から落ちる")
	}
}

func TestTachibanaOrderFrom(t *testing.T) {
	row := map[string]any{
		"sOrderNumber":        "12345",
		"sEigyouDay":          "20260903",
		"sIssueCode":          "7203",
		"sOrderStatus":        "3",
		"sOrderBaibaiKubun":   "1", // 売
		"sGenkinShinyouKubun": "2", // 信用新規
		"sOrderSuryo":         "300",
		"sOrderYakuzyouSuryo": "100",
		"sOrderYakuzyouTanka": "2500.5",
	}
	order, ok := tachibanaOrderFrom(row)
	if !ok {
		t.Fatal("注文として読めませんでした")
	}
	if *order.BrokerOrderID != "12345/20260903" {
		t.Errorf("broker_order_id = %s", *order.BrokerOrderID)
	}
	if order.Side != domain.SideSell {
		t.Errorf("売買 = %s, want SELL", order.Side)
	}
	if order.Trade != domain.TradeTypeMarginOpen {
		t.Errorf("取引種別 = %s, want MARGIN_OPEN", order.Trade)
	}
	if !order.FilledQuantity.Equal(dec("100")) || !order.Quantity.Equal(dec("300")) {
		t.Errorf("数量 = %s / %s", order.FilledQuantity, order.Quantity)
	}
	if order.AvgFillPrice == nil || !order.AvgFillPrice.Equal(dec("2500.5")) {
		t.Errorf("約定単価 = %v", order.AvgFillPrice)
	}

	// 注文番号が無い行は注文として扱わない（照会できない注文になる）
	if _, ok := tachibanaOrderFrom(map[string]any{"sIssueCode": "7203"}); ok {
		t.Error("注文番号の無い行を受け入れてしまいました")
	}
}

// 取引種別は現物へ黙って落とさない。落とすと信用の設定で現物が発注される。
func TestTradeKubunOf(t *testing.T) {
	cases := map[domain.TradeType]string{
		domain.TradeTypeCash:        kubunCash,
		"":                          kubunCash,
		domain.TradeTypeMarginOpen:  kubunMarginOpen,
		domain.TradeTypeMarginClose: kubunMarginClose,
	}
	for trade, want := range cases {
		got, err := tradeKubunOf(trade)
		if err != nil || got != want {
			t.Errorf("%s → %s (%v), want %s", trade, got, err, want)
		}
	}
	if _, err := tradeKubunOf(domain.TradeType("SOMETHING_NEW")); err == nil {
		t.Error("未知の取引種別が現物として通ってしまいました")
	}
}

func dec(v string) decimal.Decimal { return decimal.RequireFromString(v) }
