package broker

import (
	"errors"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// 発注ペイロードは取引種別・課税区分・数量の項目名を正しく埋める。
// sOrderSuryo（u 抜け）や課税区分の取り違えは受付エラーか誤発注になる。
func TestOrderPayloadFields(t *testing.T) {
	b := &TachibanaBroker{creds: testCreds()}
	limit := dec("2500")
	req := domain.OrderRequest{
		ClientOrderID: "id-1", Symbol: "7203", Side: domain.SideBuy,
		OrderType: domain.OrderTypeLimit, Quantity: dec("100"),
		LimitPrice: &limit, TaxType: domain.TaxAccountSpecific,
		Trade: domain.TradeTypeCash,
	}
	params, err := b.orderPayload(req)
	if err != nil {
		t.Fatalf("ペイロードを組めません: %v", err)
	}
	want := map[string]any{
		"sBaibaiKubun":        "3", // 買
		"sGenkinShinyouKubun": "0", // 現物
		"sZyoutoekiKazeiC":    "1", // 特定
		"sOrderSuryou":        "100",
		"sOrderPrice":         "2500",
		"sSizyouC":            "00",
		"sTatebiType":         "*",
	}
	for key, expected := range want {
		if params[key] != expected {
			t.Errorf("%s = %v, want %v", key, params[key], expected)
		}
	}
	if _, ok := params["sOrderSuryo"]; ok {
		t.Error("sOrderSuryo（u 抜け）が入っています")
	}
	if _, ok := params["aCLMKabuHensaiData"]; ok {
		t.Error("現物なのに返済の建玉指定が入っています")
	}
}

// 成行は価格 0。指値なのに価格が無ければ発注しない。
func TestOrderPayloadMarketAndMissingLimit(t *testing.T) {
	b := &TachibanaBroker{creds: testCreds()}
	market := domain.OrderRequest{
		Symbol: "7203", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		Quantity: dec("100"), Trade: domain.TradeTypeCash,
	}
	params, err := b.orderPayload(market)
	if err != nil {
		t.Fatal(err)
	}
	if params["sOrderPrice"] != "0" {
		t.Errorf("成行の価格 = %v, want 0", params["sOrderPrice"])
	}

	bad := market
	bad.OrderType = domain.OrderTypeLimit
	if _, err := b.orderPayload(bad); err == nil {
		t.Error("価格の無い指値が通ってしまいました")
	}
}

// 空売り価格規制。50 単元を超える信用新規売りは成行で出せない。
func TestOrderPayloadShortSaleMarketLimit(t *testing.T) {
	b := &TachibanaBroker{creds: testCreds()}
	req := domain.OrderRequest{
		Symbol: "7203", Side: domain.SideSell, OrderType: domain.OrderTypeMarket,
		Quantity: dec("5100"), Trade: domain.TradeTypeMarginOpen,
	}
	if _, err := b.orderPayload(req); err == nil {
		t.Fatal("51 単元以上の信用新規売りが成行で通ってしまいました")
	}

	// 50 単元ちょうどは通る
	req.Quantity = dec("5000")
	if _, err := b.orderPayload(req); err != nil {
		t.Errorf("50 単元が弾かれました: %v", err)
	}
	// 指値なら単元数に関係なく通る
	limit := dec("2500")
	req.Quantity, req.OrderType, req.LimitPrice = dec("10000"), domain.OrderTypeLimit, &limit
	if _, err := b.orderPayload(req); err != nil {
		t.Errorf("指値の信用新規売りが弾かれました: %v", err)
	}
}

// 返済は当日建てた玉から先に割り当てる（日計りは当日の玉を返す）。
func TestAllocateRepaymentPrefersToday(t *testing.T) {
	rows := []map[string]any{
		{ // 前日の売建
			fieldSideKubun: "1", fieldMarginNumber: "old",
			fieldMarginAvailable: "100", fieldMarginDay: "20260902",
		},
		{ // 当日の売建
			fieldSideKubun: "1", fieldMarginNumber: "today",
			fieldMarginAvailable: "100", fieldMarginDay: "20260903",
		},
	}
	// 返済買い（買戻し）なので売建を返す
	req := domain.OrderRequest{Symbol: "7203", Side: domain.SideBuy, Quantity: dec("100")}
	got, err := allocateRepayment(rows, req, domain.SideSell, "20260903")
	if err != nil {
		t.Fatalf("割り当てに失敗: %v", err)
	}
	if len(got) != 1 || got[0]["sTategyokuNumber"] != "today" {
		t.Errorf("当日の建玉が優先されていません: %v", got)
	}
	if got[0]["sOrderSuryou"] != "100" || got[0]["sTatebiZyuni"] != "1" {
		t.Errorf("割り当ての中身 = %v", got[0])
	}
}

// 建玉が足りなければ発注しない。足りないまま出すと返せなかった分が持ち越しになる。
func TestAllocateRepaymentRefusesWhenShort(t *testing.T) {
	rows := []map[string]any{
		{fieldSideKubun: "1", fieldMarginNumber: "a", fieldMarginAvailable: "100", fieldMarginDay: "20260903"},
	}
	req := domain.OrderRequest{Symbol: "7203", Side: domain.SideBuy, Quantity: dec("300")}
	if _, err := allocateRepayment(rows, req, domain.SideSell, "20260903"); err == nil {
		t.Fatal("建玉が足りないのに返済が通ってしまいました")
	}
}

// 反対側でない建玉（同じ向き）は返済に使わない。
func TestAllocateRepaymentSkipsWrongSide(t *testing.T) {
	rows := []map[string]any{
		{fieldSideKubun: "3", fieldMarginNumber: "long", fieldMarginAvailable: "100", fieldMarginDay: "20260903"},
	}
	req := domain.OrderRequest{Symbol: "7203", Side: domain.SideBuy, Quantity: dec("100")}
	if _, err := allocateRepayment(rows, req, domain.SideSell, "20260903"); err == nil {
		t.Fatal("買建を買戻しの対象にしてしまいました")
	}
}

// 建玉番号が取れない行は、応答の形が違うということ。黙って飛ばさない。
func TestAllocateRepaymentRequiresPositionNumber(t *testing.T) {
	rows := []map[string]any{
		{fieldSideKubun: "1", fieldMarginAvailable: "100", fieldMarginDay: "20260903"},
	}
	req := domain.OrderRequest{Symbol: "7203", Side: domain.SideBuy, Quantity: dec("100")}
	_, err := allocateRepayment(rows, req, domain.SideSell, "20260903")
	var unverified *ErrUnverifiedResponse
	if !errors.As(err, &unverified) {
		t.Fatalf("建玉番号の欠落が検出されません: %v", err)
	}
}

// testCreds は発注ペイロードの検証に足りる最小の認証情報。
func testCreds() *credentials.TachibanaCredentials {
	return &credentials.TachibanaCredentials{OrderPassword: "x"}
}
