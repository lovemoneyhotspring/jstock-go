package broker

// 実機で確かめた応答の形（docs/BROKER_VERIFY.md の 2026-09-11 / 09-14 の節）を模型に仕込み、
// 発注・照会の経路を通信なしで確かめる。行の項目名はそのときの応答から写してある。

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// TestMain は発注口のレート制限の待ちを止める（模型に数十本送るテストが秒単位で遅くならないように）。
// 待ちが要求されること自体は TestRequestEndpointIsRateLimited が別に確かめる。
func TestMain(m *testing.M) {
	requestLimiter().SetSleep(func(time.Duration) {})
	orderLimiter().SetSleep(func(time.Duration) {})
	os.Exit(m.Run())
}

// newFixtureBroker は模型に繋いだブローカーと模型。
func newFixtureBroker(t *testing.T) (*TachibanaBroker, *fakeTachibana) {
	t.Helper()
	dir := t.TempDir()
	keyPath, pub := writeTestKey(t, dir)
	fake := newFakeTachibana(t, pub)
	return newSessionTestBroker(t, fake, keyPath, dir), fake
}

func okResponse(extra map[string]any) map[string]any {
	res := map[string]any{"p_errno": "0", "sResultCode": "0"}
	for k, v := range extra {
		res[k] = v
	}
	return res
}

// marginRowFixture は信用建玉（CLMShinyouTategyokuList）の 1 行。2026-09-14 の実機の項目名。
func marginRowFixture(symbol, side, number, qty, day string) map[string]any {
	return map[string]any{
		"sOrderIssueCode": symbol, "sOrderBaibaiKubun": side, "sOrderTategyokuNumber": number,
		"sOrderTategyokuSuryou": qty, "sOrderHensaiKanouSuryou": qty,
		"sOrderTategyokuTanka": "2345", "sOrderHyoukaTanka": "2346", "sOrderTategyokuDay": day,
		"sOrderTategyokuKizituDay": "20270312", "sOrderZyoutoekiKazeiC": "1",
		"sOrderBensaiKubun": "26", "sOrderTateTesuryou": "0", "sOrderKanrihi": "0", "sOrderZyunHibu": "0",
	}
}

// cashOrderRowFixture は注文一覧（CLMOrderList）の 1 行。2026-09-11 の現物 1 株（563A・指値 999・約定 998）。
func cashOrderRowFixture() map[string]any {
	return map[string]any{
		"sOrderOrderNumber": "11010971", "sOrderSikkouDay": "20260911", "sOrderIssueCode": "563A",
		"sOrderBaibaiKubun": "3", "sGenkinSinyouKubun": "0", "sOrderOrderPriceKubun": "2",
		"sOrderOrderPrice": "999", "sOrderOrderSuryou": "1", "sOrderCurrentSuryou": "0",
		"sOrderYakuzyouSuryo": "1", "sOrderYakuzyouPrice": "998", "sOrderStatusCode": "10",
		"sOrderOrderDateTime": "20260911114512", "sOrderGyakusasiOrderType": "0", "sOrderTriggerType": "0",
	}
}

// cashOrderDetailFixture は同じ注文の単品照会（CLMOrderListDetail）。接頭辞の無い項目名。
func cashOrderDetailFixture() map[string]any {
	return okResponse(map[string]any{
		"sOrderNumber": "11010971", "sEigyouDay": "20260911", "sIssueCode": "563A",
		"sOrderBaibaiKubun": "3", "sGenkinSinyouKubun": "0", "sOrderOrderPriceKubun": "2",
		"sOrderOrderPrice": "999", "sOrderOrderSuryou": "1", "sOrderCurrentSuryou": "0",
		"sYakuzyouSuryou": "1", "sYakuzyouPrice": "998", "sOrderStatusCode": "10",
		"sOrderOrderDateTime": "20260911114512", "sGyakusasiOrderType": "0", "sTriggerType": "0",
	})
}

func cashOrderRequest(id string) domain.OrderRequest {
	limit := dec("999")
	return domain.OrderRequest{
		ClientOrderID: id, Symbol: "563A", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		Quantity: dec("1"), LimitPrice: &limit, TaxType: domain.TaxAccountSpecific, Trade: domain.TradeTypeCash,
	}
}

// --- 1. 結果コードの無い発注応答は「拒否」ではなく「結果不明」 ------------------------------

// sResultCode が無い応答を拒否と読むと、呼び出し側が REJECTED にして送り直し、届いていた
// 場合に二重発注になる。ErrUnverifiedResponse（結果不明＝PENDING のまま）で止める。
func TestPlaceWithoutResultCodeIsUnverifiedNotRejected(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmNewOrder] = map[string]any{"p_errno": "0"}
	fake.responses[clmCancelOrder] = map[string]any{"p_errno": "0"}
	fake.responses[clmCorrectOrder] = map[string]any{"p_errno": "0"}

	_, err := b.Place(cashOrderRequest("c1"))
	var rejected *OrderRejectedError
	var unverified *ErrUnverifiedResponse
	if errors.As(err, &rejected) {
		t.Fatalf("結果コードの無い応答が拒否になった（送り直されて二重発注になる）: %v", err)
	}
	if !errors.As(err, &unverified) {
		t.Fatalf("ErrUnverifiedResponse ではない: %v", err)
	}
	id := "1/20260914"
	if err := b.Cancel("c1", &id); !errors.As(err, &unverified) {
		t.Errorf("取消: 結果コードの無い応答が結果不明になっていない: %v", err)
	}
	if err := b.CorrectStop("c1", &id, domain.StopSpec{Trigger: dec("100")}); !errors.As(err, &unverified) {
		t.Errorf("訂正: 結果コードの無い応答が結果不明になっていない: %v", err)
	}
	// 業務エラーは今までどおり拒否
	fake.responses[clmNewOrder] = map[string]any{"p_errno": "0", "sResultCode": "12115", "sResultText": "だめ"}
	if _, err := b.Place(cashOrderRequest("c2")); !errors.As(err, &rejected) {
		t.Errorf("業務エラーが拒否になっていない: %v", err)
	}
}

// --- 2. 時価・ニュースの配列のキーが無ければ止める --------------------------------------------

func TestMarketPriceMissingRowsKeyIsAnError(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.priceOmitRows = true
	_, err := b.marketPriceBatch([]string{"7203"}, MarketPriceColumns)
	var unverified *ErrUnverifiedResponse
	if !errors.As(err, &unverified) {
		t.Fatalf("配列のキーが無い時価応答が 0 行として通った（全銘柄が「気配なし」になる）: %v", err)
	}
	if _, err := b.MarketPricesRaw([]string{"7203"}, ""); err == nil {
		t.Fatal("MarketPricesRaw が成功した")
	}
}

func TestNewsMissingRowsKeyIsAnError(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmGetNews] = map[string]any{"p_errno": "0"}
	_, err := b.News("20260914")
	var unverified *ErrUnverifiedResponse
	if !errors.As(err, &unverified) {
		t.Fatalf("配列のキーが無いニュース応答が 0 件として通った: %v", err)
	}
	// 該当なしは空文字で返る（他の一覧電文と同じ）→ 0 件
	fake.responses[clmGetNews] = map[string]any{"p_errno": "0", newsKey: ""}
	if items, err := b.News("20260914"); err != nil || len(items) != 0 {
		t.Errorf("該当なし: items=%v err=%v", items, err)
	}
	// 行があれば読める
	head := base64.StdEncoding.EncodeToString([]byte("Rating+1"))
	fake.responses[clmGetNews] = map[string]any{"p_errno": "0", newsKey: []any{
		map[string]any{"p_ID": "1", "p_TM": "0710", "p_GNL": "60220|60230", "p_ISL": "7203|9984", "p_HDL": head, "p_TX": ""},
	}}
	items, err := b.News("20260914")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if items[0].Headline != "Rating+1" || !items[0].HasGenre(GenreRatingUp) || len(items[0].Codes) != 2 {
		t.Errorf("読めた中身が違う: %+v", items[0])
	}
	if sent := fake.lastPayload(clmGetNews); sent == nil || sent["p_DT"] != "20260914" {
		t.Errorf("送った電文: %v", sent)
	}
}

// --- 3. p_errno の値で扱いを分ける -------------------------------------------------------

// -1（引数エラー）: 基盤が受け付ける前に弾いた。発注は「送っていない」。セッションは捨てない。
func TestArgumentErrorOnNewOrderIsNotSentAndKeepsSession(t *testing.T) {
	b, fake := newFixtureBroker(t)
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatal(err)
	}
	fake.failNext, fake.failErrno = 1, pErrnoArgument
	_, err := b.Place(cashOrderRequest("c1"))
	var notSent *ErrNotSent
	if !errors.As(err, &notSent) {
		t.Fatalf("引数エラーが ErrNotSent でない: %v", err)
	}
	var platform *ErrPlatform
	if !errors.As(err, &platform) || platform.Errno != pErrnoArgument {
		t.Errorf("ErrPlatform が包まれていない: %v", err)
	}
	if fake.countCLM(clmNewOrder) != 1 {
		t.Errorf("発注が送り直されている: %v", fake.clmIDs)
	}
	if _, ok := readSessionFile(b.sessionFilePath()); !ok {
		t.Error("引数エラーでセッションを捨てている")
	}
	if fake.logins != 1 {
		t.Errorf("logins=%d（再ログインは不要）", fake.logins)
	}
}

// 6（p_no の逆転）: p_no の検査は受付より前。発注は「送っていない」。セッションは捨てない。
func TestPNoOrderErrorOnNewOrderIsNotSentAndKeepsSession(t *testing.T) {
	b, fake := newFixtureBroker(t)
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatal(err)
	}
	fake.failNext, fake.failErrno = 1, pErrnoPNoOrder
	_, err := b.Place(cashOrderRequest("c1"))
	var notSent *ErrNotSent
	if !errors.As(err, &notSent) {
		t.Fatalf("p_no の逆転が ErrNotSent でない: %v", err)
	}
	if fake.countCLM(clmNewOrder) != 1 {
		t.Errorf("発注が送り直されている: %v", fake.clmIDs)
	}
	if _, ok := readSessionFile(b.sessionFilePath()); !ok {
		t.Error("p_no の逆転でセッションを捨てている")
	}
	if fake.logins != 1 {
		t.Errorf("logins=%d（再ログインは不要）", fake.logins)
	}
}

// -62（時間外）: 送り直さず、セッションも捨てない。
func TestOutsideHoursKeepsSessionAndDoesNotResend(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.failNext, fake.failErrno = 1, pErrnoOutsideHours
	_, err := b.postRequest(clmBalanceSummary, nil)
	var platform *ErrPlatform
	if !errors.As(err, &platform) || platform.Errno != pErrnoOutsideHours {
		t.Fatalf("時間外が ErrPlatform(-62) でない: %v", err)
	}
	var session *ErrSession
	if errors.As(err, &session) {
		t.Error("時間外が ErrSession になっている")
	}
	if fake.countCLM(clmBalanceSummary) != 1 {
		t.Errorf("照会が送り直されている: %v", fake.clmIDs)
	}
	if _, ok := readSessionFile(b.sessionFilePath()); !ok {
		t.Error("時間外でセッションを捨てている")
	}
	// 発注で -62 なら ErrNotSent にはしない（届いていないとは言い切れない）
	fake.failNext = 1
	_, err = b.Place(cashOrderRequest("c1"))
	var notSent *ErrNotSent
	if errors.As(err, &notSent) {
		t.Errorf("時間外の発注を「送っていない」にしている: %v", err)
	}
	if !errors.As(err, &platform) {
		t.Errorf("時間外の発注が ErrPlatform でない: %v", err)
	}
}

// 2（切断）: 捨ててログインし直し、照会だけ送り直す（既存の挙動）。
func TestSessionLostRelogsIn(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.failNext, fake.failErrno = 1, pErrnoSessionLost
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatalf("切断後の送り直しが失敗: %v", err)
	}
	if fake.logins != 2 || fake.countCLM(clmBalanceSummary) != 2 {
		t.Errorf("logins=%d sent=%v", fake.logins, fake.clmIDs)
	}
}

// --- 4. 同じ client_order_id は 2 度送らない --------------------------------------------------

func TestPlaceReturnsExistingAckForRepeatedClientOrderID(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmNewOrder] = okResponse(map[string]any{"sOrderNumber": "11010971", "sEigyouDay": "20260911"})
	first, err := b.Place(cashOrderRequest("c1"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Place(cashOrderRequest("c1"))
	if err != nil {
		t.Fatal(err)
	}
	if fake.countCLM(clmNewOrder) != 1 {
		t.Fatalf("同じ client_order_id で 2 度送っている: %v", fake.clmIDs)
	}
	if second.BrokerOrderID == nil || *second.BrokerOrderID != *first.BrokerOrderID || second.ClientOrderID != "c1" {
		t.Errorf("2 度目の ack が 1 度目と違う: %+v / %+v", first, second)
	}
	// 別の ID は送る
	if _, err := b.Place(cashOrderRequest("c2")); err != nil || fake.countCLM(clmNewOrder) != 2 {
		t.Errorf("別の ID が送られていない: %v %v", err, fake.clmIDs)
	}
}

// --- 5. ログインの業務エラーと壊れたセッションファイル ---------------------------------------

func TestLoginBusinessErrorIsReportedBeforeDecrypt(t *testing.T) {
	b, fake := newFixtureBroker(t)
	// 模型は UTF-8 で返し、ブローカーは Shift_JIS として読むので、比べる文字列は ASCII にする
	fake.loginExtra = map[string]any{"sResultCode": "-1", "sResultText": "auth failed", "sUrlRequest": ""}
	_, err := b.postRequest(clmBalanceSummary, nil)
	if err == nil {
		t.Fatal("業務エラーのログインが通った")
	}
	if strings.Contains(err.Error(), "RSA OAEP") {
		t.Errorf("業務エラーが復号エラーとして出ている: %v", err)
	}
	if !strings.Contains(err.Error(), "-1") || !strings.Contains(err.Error(), "auth failed") {
		t.Errorf("結果コードと理由が出ていない: %v", err)
	}
}

func TestCorruptedSessionFileTriggersRelogin(t *testing.T) {
	b, fake := newFixtureBroker(t)
	path := b.sessionFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"p_no\": 5, \"url_request\": "), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.postRequest(clmBalanceSummary, nil); err != nil {
		t.Fatalf("壊れたセッションファイルから立ち直れない: %v", err)
	}
	if fake.logins != 1 {
		t.Errorf("logins=%d, want 1", fake.logins)
	}
	if saved, ok := readSessionFile(path); !ok || saved.URLRequest == "" {
		t.Error("ログインし直したセッションが書かれていない")
	}
}

// --- 6. 警告（sWarningCode / sWarningText）を残す --------------------------------------------

func TestWarningInResponseIsLogged(t *testing.T) {
	b, fake := newFixtureBroker(t)
	log := &recordingLogger{}
	b.SetLogger(log)
	fake.responses[clmBalanceSummary] = okResponse(map[string]any{
		fieldCashBuyingPower: "3000", "sWarningCode": "31001", "sWarningText": "notice"}) // ASCII（Shift_JIS で読まれるため）
	if _, err := b.GetBalance(); err != nil {
		t.Fatal(err)
	}
	warns := log.find("warn", "broker.warning")
	if len(warns) != 1 || warns[0].extra["warning_code"] != "31001" || warns[0].extra["warning_text"] != "notice" {
		t.Fatalf("警告が残っていない: %+v", warns)
	}
	// "0" と空は警告ではない
	fake.responses[clmBalanceSummary] = okResponse(map[string]any{fieldCashBuyingPower: "3000", "sWarningCode": "0", "sWarningText": ""})
	if _, err := b.GetBalance(); err != nil {
		t.Fatal(err)
	}
	if got := log.find("warn", "broker.warning"); len(got) != 1 {
		t.Errorf("警告コード 0 で警告を残している: %+v", got)
	}
}

// --- 7. ニュース本文の「+」 -----------------------------------------------------------------

func TestDecodeNewsTextKeepsPlus(t *testing.T) {
	// %89%7E は Shift_JIS の「円」。「+」はそのまま「+」（QueryUnescape だと空白になる）
	raw := "%89%7E+1%25+A%2BB"
	got := DecodeNewsText(base64.StdEncoding.EncodeToString([]byte(raw)))
	if got != "円+1%+A+B" {
		t.Errorf("DecodeNewsText = %q, want %q", got, "円+1%+A+B")
	}
}

// --- 9. 発注口のレート制限 -------------------------------------------------------------------

func TestRequestEndpointIsRateLimited(t *testing.T) {
	lim := requestLimiter()
	var waited []time.Duration
	lim.SetSleep(func(d time.Duration) { waited = append(waited, d) })
	t.Cleanup(func() { lim.SetSleep(func(time.Duration) {}) })

	b, fake := newFixtureBroker(t)
	for i := 0; i < lim.Limit().Calls+1; i++ {
		if _, err := b.postRequest(clmOrderDetail, map[string]any{"sOrderNumber": "1", "sEigyouDay": "20260914"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(waited) == 0 {
		t.Fatalf("上限（%s）を超えても待ちが要求されない", lim.Limit())
	}
	if fake.countCLM(clmOrderDetail) != lim.Limit().Calls+1 {
		t.Errorf("待ったうえで全部送るはず: %v", fake.clmIDs)
	}
	// 注文は別枠（余力の照会が注文を遅らせない）。枠はプロセスに 1 つで、先のテストの注文が
	// 残りを減らしているので、ここでは電文と枠の対応だけを確かめる
	if requestLimiterFor(clmNewOrder) != orderLimiter() || requestLimiterFor(clmCancelOrder) != orderLimiter() ||
		requestLimiterFor(clmCorrectOrder) != orderLimiter() {
		t.Error("注文（新規・訂正・取消）が注文の枠を使っていない")
	}
	for _, clm := range []string{clmBalanceSummary, clmCashPositions, clmMarginPositions, clmOrderList, clmOrderDetail} {
		if requestLimiterFor(clm) != lim {
			t.Errorf("%s が照会の枠を使っていない", clm)
		}
	}
	// 時価の口はこの制限を使わない
	before := len(waited)
	if _, err := b.MarketPricesRaw([]string{"7203"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(waited) != before {
		t.Error("時価問合が発注口の制限を消費している")
	}
}

// --- 10. プローブの state はリポジトリ root ------------------------------------------------

func TestProbeStateDirResolvesToRepoRoot(t *testing.T) {
	got := probeStateDir(t, "state")
	if !filepath.IsAbs(got) {
		t.Fatalf("絶対パスでない: %s", got)
	}
	if strings.Contains(got, filepath.Join("pkg", "wbcore", "broker")) {
		t.Fatalf("パッケージの下に解決している: %s", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(got), "go.mod")); err != nil {
		t.Errorf("go.mod の隣ではない: %s", got)
	}
	abs := t.TempDir()
	if probeStateDir(t, abs) != abs {
		t.Error("在る絶対パスを変えている")
	}
	// 設定は相対パスを作業ディレクトリ（パッケージの下）から絶対化して渡してくる。無ければ root へ
	cwd, _ := os.Getwd()
	if missing := probeStateDir(t, filepath.Join(cwd, "無い-state")); strings.Contains(missing, filepath.Join("pkg", "wbcore", "broker")) {
		t.Errorf("無い絶対パスをパッケージの下のまま返している: %s", missing)
	}
}

// --- 11. 信用返済の発注 ------------------------------------------------------------------

func TestPlaceMarginRepaymentSendsPositionAllocation(t *testing.T) {
	b, fake := newFixtureBroker(t)
	today := b.today().Format("20060102")
	fake.responses[clmMarginPositions] = okResponse(map[string]any{
		marginPositionsKey: []any{marginRowFixture("2012", "3", "14012403", "10", today)}})
	fake.responses[clmNewOrder] = okResponse(map[string]any{"sOrderNumber": "14012415", "sEigyouDay": "20260914"})

	ack, err := b.Place(domain.OrderRequest{
		ClientOrderID: "close-1", Symbol: "2012", Side: domain.SideSell, OrderType: domain.OrderTypeMarket,
		Quantity: dec("10"), TaxType: domain.TaxAccountSpecific, Trade: domain.TradeTypeMarginClose,
	})
	if err != nil {
		t.Fatalf("返済が通らない: %v", err)
	}
	if ack.BrokerOrderID == nil || *ack.BrokerOrderID != "14012415/20260914" {
		t.Errorf("BrokerOrderID = %v, want 14012415/20260914", ack.BrokerOrderID)
	}
	if q := fake.lastPayload(clmMarginPositions); q == nil || q["sIssueCode"] != "2012" {
		t.Errorf("建玉の照会が銘柄指定になっていない: %v", q)
	}
	sent := fake.lastPayload(clmNewOrder)
	if sent == nil {
		t.Fatal("発注の電文が送られていない")
	}
	for key, want := range map[string]any{
		"sGenkinShinyouKubun": "4", "sBaibaiKubun": "1", "sTatebiType": "1", "sIssueCode": "2012",
		"sOrderSuryou": "10", "sOrderPrice": "0",
	} {
		if sent[key] != want {
			t.Errorf("%s = %v, want %v", key, sent[key], want)
		}
	}
	hensai, _ := sent["aCLMKabuHensaiData"].([]any)
	if len(hensai) != 1 {
		t.Fatalf("aCLMKabuHensaiData = %v, want 1 行", sent["aCLMKabuHensaiData"])
	}
	first, _ := hensai[0].(map[string]any)
	if first["sTategyokuNumber"] != "14012403" || first["sOrderSuryou"] != "10" || first["sTatebiZyuni"] != "1" {
		t.Errorf("建玉の指定 = %v", first)
	}
	if b.nativeOrderID("close-1") != "14012415/20260914" {
		t.Error("注文番号を控えていない")
	}
}

// --- 12. 信用建玉の符号と脚 ----------------------------------------------------------------

func TestMarginPositionsSignAndBrokerPositionID(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmMarginPositions] = okResponse(map[string]any{marginPositionsKey: []any{
		marginRowFixture("7203", "1", "S1", "300", "20260914"), // 売建
		marginRowFixture("2012", "3", "L1", "10", "20260914"),  // 買建
	}})
	positions, err := b.MarginPositions()
	if err != nil {
		t.Fatal(err)
	}
	bySymbol := map[string]domain.Position{}
	for _, p := range positions {
		bySymbol[p.Symbol] = p
	}
	short := bySymbol["7203"]
	if !short.Quantity.Equal(dec("-300")) || !short.AvailableQuantity.Equal(dec("-300")) {
		t.Errorf("売建は負: 数量 %s 返済可能 %s", short.Quantity, short.AvailableQuantity)
	}
	if short.BrokerPositionID != "S1" || short.Trade != domain.TradeTypeMarginOpen || !short.CostPrice.Equal(dec("2345")) {
		t.Errorf("売建の中身: %+v", short)
	}
	long := bySymbol["2012"]
	if !long.Quantity.Equal(dec("10")) || !long.AvailableQuantity.Equal(dec("10")) || long.BrokerPositionID != "L1" {
		t.Errorf("買建の中身: %+v", long)
	}
}

// 現物 300 株と売建 300 株は別の脚。銘柄だけで束ねると 0 株になり、売建の持ち越しが消える。
func TestPositionsByLegKeepsCashAndShortSeparate(t *testing.T) {
	b, fake := newFixtureBroker(t)
	fake.responses[clmCashPositions] = okResponse(map[string]any{cashPositionsKey: []any{map[string]any{
		fieldCashIssue: "7203", fieldCashQty: "300", fieldCashAvailable: "300",
		fieldCashCost: "2500", fieldCashLast: "2510", fieldCashTax: "1",
	}}})
	fake.responses[clmMarginPositions] = okResponse(map[string]any{marginPositionsKey: []any{
		marginRowFixture("7203", "1", "S1", "300", "20260914"),
	}})
	legs := PositionsByLeg(b)
	if legs.CashErr != nil || legs.MarginErr != nil {
		t.Fatalf("照会に失敗: %v / %v", legs.CashErr, legs.MarginErr)
	}
	if got := legs.Legs(); len(got) != 2 {
		t.Fatalf("脚が %d 本（現物と売建で 2 本のはず）: %+v", len(got), got)
	}
	cash, ok := legs.At(LegOf("7203", domain.TradeTypeCash, false))
	if !ok || !cash.Quantity.Equal(dec("300")) || !cash.CostPrice.Equal(dec("2500")) {
		t.Errorf("現物の脚 = %+v ok=%v", cash, ok)
	}
	short, ok := legs.At(LegOf("7203", domain.TradeTypeMarginOpen, true))
	if !ok || !short.Quantity.Equal(dec("300")) {
		t.Errorf("売建の脚 = %+v ok=%v（数量は正で持つ）", short, ok)
	}
	// 向きが売りなら取引区分が空でも信用（現物に売り玉は無い）
	if leg := LegOf("7203", "", true); !leg.Margin || !leg.Short {
		t.Errorf("LegOf(空, 売り) = %+v", leg)
	}
	if leg := LegOf("7203", "", false); leg.Margin {
		t.Errorf("LegOf(空, 買い) を信用にしている: %+v", leg)
	}
	// 銘柄だけで束ねると 0 になる（脚で分ける理由）
	all := append([]domain.Position{}, mustPositions(t, b.GetPositions)...)
	all = append(all, mustPositions(t, b.MarginPositions)...)
	if merged := PositionsBySymbolHelper(all); !merged["7203"].Quantity.IsZero() {
		t.Errorf("銘柄だけの合算 = %s（0 になるはず。脚で分ける前提の確認）", merged["7203"].Quantity)
	}
}

func mustPositions(t *testing.T, fetch func() ([]domain.Position, error)) []domain.Position {
	t.Helper()
	positions, err := fetch()
	if err != nil {
		t.Fatal(err)
	}
	return positions
}

// --- 13. 単品照会 --------------------------------------------------------------------------

func TestGetOrderThreeCases(t *testing.T) {
	b, fake := newFixtureBroker(t)
	id := "11010971/20260911"

	// 該当なし（991005）→ (nil, nil)
	fake.responses[clmOrderDetail] = map[string]any{"p_errno": "0", "sResultCode": orderNotFoundCode, "sResultText": "該当する注文はありません"}
	if o, err := b.GetOrder("c1", &id); err != nil || o != nil {
		t.Errorf("該当なし: o=%v err=%v", o, err)
	}
	// 業務エラー → エラー
	fake.responses[clmOrderDetail] = map[string]any{"p_errno": "0", "sResultCode": "1", "sResultText": "だめ"}
	if _, err := b.GetOrder("c1", &id); err == nil {
		t.Error("業務エラーが nil で通った")
	}
	// 行あり → 単品照会の項目名で読める
	fake.responses[clmOrderDetail] = cashOrderDetailFixture()
	o, err := b.GetOrder("c1", &id)
	if err != nil || o == nil {
		t.Fatalf("o=%v err=%v", o, err)
	}
	if sent := fake.lastPayload(clmOrderDetail); sent["sOrderNumber"] != "11010971" || sent["sEigyouDay"] != "20260911" {
		t.Errorf("照会の電文: %v", sent)
	}
	if o.ClientOrderID != "c1" || *o.BrokerOrderID != id || o.Symbol != "563A" {
		t.Errorf("識別子: %+v", o)
	}
	if !o.Quantity.Equal(dec("1")) || !o.FilledQuantity.Equal(dec("1")) || o.Status != domain.OrderStatusFilled {
		t.Errorf("数量 %s 約定 %s 状態 %s", o.Quantity, o.FilledQuantity, o.Status)
	}
	if o.AvgFillPrice == nil || !o.AvgFillPrice.Equal(dec("998")) || o.LimitPrice == nil || !o.LimitPrice.Equal(dec("999")) {
		t.Errorf("約定単価 %v 指値 %v", o.AvgFillPrice, o.LimitPrice)
	}
	if o.Side != domain.SideBuy || o.Trade != domain.TradeTypeCash || o.OrderType != domain.OrderTypeLimit {
		t.Errorf("売買 %s 取引 %s 種別 %s", o.Side, o.Trade, o.OrderType)
	}
	// 注文番号が分からなければエラー（nil にすると「無いから再送してよい」と誤認される）
	if _, err := b.GetOrder("unknown", nil); err == nil {
		t.Error("注文番号の分からない照会が nil で通った")
	}
}

// --- 14. 注文一覧 --------------------------------------------------------------------------

func TestOrderListReadsRowsAndSkipsUnreadableOnes(t *testing.T) {
	b, fake := newFixtureBroker(t)
	log := &recordingLogger{}
	b.SetLogger(log)
	bad := cashOrderRowFixture()
	bad["sOrderOrderNumber"], bad["sOrderBaibaiKubun"] = "999", "5" // 現渡は扱わない
	noNumber := cashOrderRowFixture()
	delete(noNumber, "sOrderOrderNumber")
	fake.responses[clmOrderList] = okResponse(map[string]any{orderListKey: []any{cashOrderRowFixture(), bad, noNumber}})
	b.rememberNativeOrderID("c1", "11010971/20260911")

	orders, err := b.GetOpenOrders()
	if err != nil {
		t.Fatal(err)
	}
	if sent := fake.lastPayload(clmOrderList); sent["sOrderSyoukaiStatus"] != openOrderStatusFilter {
		t.Errorf("未約定の絞り込みが無い: %v", sent)
	}
	if len(orders) != 1 {
		t.Fatalf("読めた注文 %d 件（読める 1 行だけ）: %+v", len(orders), orders)
	}
	o := orders[0]
	if o.ClientOrderID != "c1" || *o.BrokerOrderID != "11010971/20260911" {
		t.Errorf("client_order_id の逆引き: %+v", o)
	}
	if o.Symbol != "563A" || !o.Quantity.Equal(dec("1")) || !o.FilledQuantity.Equal(dec("1")) ||
		o.AvgFillPrice == nil || !o.AvgFillPrice.Equal(dec("998")) || o.Status != domain.OrderStatusFilled {
		t.Errorf("行の中身: %+v", o)
	}
	if o.CreatedAt == nil || o.CreatedAt.In(jst()).Format("20060102150405") != "20260911114512" {
		t.Errorf("発注時刻: %v", o.CreatedAt)
	}
	warns := log.find("warn", "broker.order_row_unreadable")
	if len(warns) != 1 || warns[0].extra["order_number"] != "999" {
		t.Errorf("読めない行の警告が 1 件（999）残るはず: %+v", warns)
	}
	// 同一プロセスで出していない注文は broker_order_id を client_order_id に使う
	b.nativeOrderIDs = nil
	orders, _ = b.GetOpenOrders()
	if len(orders) != 1 || orders[0].ClientOrderID != "11010971/20260911" {
		t.Errorf("逆引きできない注文の client_order_id: %+v", orders)
	}
}

func TestGetOrderHistoryIsTodayOnly(t *testing.T) {
	b, fake := newFixtureBroker(t)
	log := &recordingLogger{}
	b.SetLogger(log)
	fake.responses[clmOrderList] = okResponse(map[string]any{orderListKey: []any{cashOrderRowFixture()}})
	today := b.today()
	yesterday := today.AddDate(0, 0, -1)

	// 終わりが今日より前 → 何も送らず nil, nil
	orders, err := b.GetOrderHistory(yesterday, yesterday)
	if err != nil || orders != nil {
		t.Errorf("前日までの期間: orders=%v err=%v", orders, err)
	}
	if fake.countCLM(clmOrderList) != 0 {
		t.Error("前日までの期間なのに一覧を送った")
	}
	if len(log.find("warn", "broker.history_today_only")) != 1 {
		t.Error("前日以前を含む期間の警告が無い")
	}
	// 今日を含む → 全件（絞り込みなし）
	orders, err = b.GetOrderHistory(yesterday, today)
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders=%v err=%v", orders, err)
	}
	if sent := fake.lastPayload(clmOrderList); sent["sOrderSyoukaiStatus"] != "" {
		t.Errorf("履歴は絞り込みなしのはず: %v", sent)
	}
}

// --- 15. 売買単位のマスタ（sResultCode が無い電文） ----------------------------------------

func TestLotSizesFromMasterWithoutResultCode(t *testing.T) {
	b, fake := newFixtureBroker(t)
	// 実機の応答: aCLMStkIssueMstKabu / p_errno / p_err / p_no / p_sd_date / p_rv_date / sCLMID（sResultCode 無し）
	fake.responses[clmStockMaster] = map[string]any{
		"p_errno": "0", "p_err": "", "sCLMID": clmStockMaster, "p_rv_date": "2026.09.14-09:00:00.000",
		stockMasterKey: []any{
			map[string]any{"sIssueCode": "7203", "sIssueName": "トヨタ自動車", "sBaibaiTani": "100"},
			map[string]any{"sIssueCode": "1629", "sIssueName": "NEXT FUNDS 商社", "sBaibaiTani": "10"},
			map[string]any{"sIssueCode": "", "sBaibaiTani": "100"},
		},
	}
	got := b.LotSizes([]string{"7203", "1629", "9999"})
	if !got["7203"].Equal(decimal.NewFromInt(100)) || !got["1629"].Equal(decimal.NewFromInt(10)) {
		t.Errorf("売買単位 = %v", got)
	}
	if _, ok := got["9999"]; ok {
		t.Error("マスタに無い銘柄がキーごと入っている")
	}
	// 2 度目はマスタを取り直さない
	b.LotSizes([]string{"7203"})
	if fake.countCLM(clmStockMaster) != 1 {
		t.Errorf("マスタを %d 回取っている", fake.countCLM(clmStockMaster))
	}
	if !b.cachedLotSize("1629").Equal(decimal.NewFromInt(10)) {
		t.Error("発注時の空売り規制が引く単元（cachedLotSize）に入っていない")
	}
	// 配列のキーが無ければ空（警告付き）——0 件と読まない
	c, fake2 := newFixtureBroker(t)
	fake2.responses[clmStockMaster] = map[string]any{"p_errno": "0"}
	if got := c.LotSizes([]string{"7203"}); len(got) != 0 {
		t.Errorf("形の違うマスタ応答から値が出た: %v", got)
	}
}
