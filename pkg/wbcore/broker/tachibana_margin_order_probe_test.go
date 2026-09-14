package broker

// 信用の発注経路を実機で 1 周させる調べもの（docs/BROKER_VERIFY.md の信用 4 点と手順 5 e）。
// **実際に発注する。** 1 単元を信用で買い建て、同じ実行の中で返済まで済ませる。
//
//	WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
//	  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
//	  TACHIBANA_MARGIN_ORDER_PROBE=2012 go test ./pkg/wbcore/broker -run TestMarginOrderProbe -v -count=1
//
// 安全のための約束:
//   - 買建だけ（空売りはしない）。1 単元・見積り 5,000 円まで（TACHIBANA_MARGIN_ORDER_MAX_YEN で変える）
//   - その銘柄に信用建玉が既にあれば何も送らない（他の玉を返済しないため）
//   - 新規は売気配、返済は買気配の指値（成行にしない）
//   - 返済の逆指値は発火しない水準（買気配 −3%）に置き、訂正して取消す
//   - 途中で落ちても、建った玉は最後に必ず返済を試みる
import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

func TestMarginOrderProbe(t *testing.T) {
	symbol := os.Getenv("TACHIBANA_MARGIN_ORDER_PROBE")
	if symbol == "" {
		t.Skip("TACHIBANA_MARGIN_ORDER_PROBE=2012 を立てたときだけ動かす（実際に発注する）")
	}
	maxYen := decimal.NewFromInt(5000)
	if v := os.Getenv("TACHIBANA_MARGIN_ORDER_MAX_YEN"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		maxYen = decimal.NewFromInt(n)
	}
	app := settings.LoadAppSettings()
	creds, err := credentials.LoadTachibanaCredentials(app.Env, app.DotenvMap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTachibanaBroker(app.Env, creds, app.StateDir)
	if err != nil {
		t.Fatal(err)
	}

	// 0. 売買単位・気配・既存の建玉
	lot := b.LotSizes([]string{symbol})[symbol]
	if !lot.IsPositive() {
		t.Fatalf("%s の売買単位が取れません", symbol)
	}
	quote, err := b.MarketPrices([]string{symbol})
	if err != nil {
		t.Fatal(err)
	}
	q := quote[symbol]
	if !q.Ask.IsPositive() || !q.Bid.IsPositive() {
		t.Fatalf("%s の気配が無い（買 %s / 売 %s）。ザラ場中に回す", symbol, q.Bid, q.Ask)
	}
	estimate := lot.Mul(q.Ask)
	t.Logf("%s 単元 %s  買気配 %s  売気配 %s  見積り %s 円（上限 %s 円）", symbol, lot, q.Bid, q.Ask, estimate, maxYen)
	if estimate.GreaterThan(maxYen) {
		t.Fatalf("見積りが上限を超えるので送りません")
	}
	before, err := b.marginRows(symbol)
	if err != nil {
		t.Fatalf("信用建玉を照会できません: %v", err)
	}
	if len(before) > 0 {
		t.Fatalf("%s に信用建玉が既に %d 行あるので送りません", symbol, len(before))
	}

	seed := "margin-probe|" + clock.NowUTC().Format(time.RFC3339Nano)
	opened := false
	// 途中で落ちても建玉を残さない
	defer func() {
		if !opened {
			return
		}
		rows, err := b.marginRows(symbol)
		if err != nil || len(rows) == 0 {
			return
		}
		t.Logf("⚠ 後始末: %s の建玉が残っているので返済を試みる", symbol)
		closeLong(t, b, symbol, lot, seed+"|cleanup")
	}()

	// 1. 信用新規買い（売気配の指値）
	openReq, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed+"|open", symbol, domain.SideBuy, lot), symbol,
		domain.SideBuy, domain.OrderTypeLimit, lot, &q.Ask,
		domain.TaxAccountSpecific, "信用新規の実機検証", domain.TradeTypeMarginOpen)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := b.Place(openReq)
	if err != nil {
		t.Fatalf("❌ 信用新規: %v", err)
	}
	opened = true
	t.Logf("✅ 信用新規: 受理 注文番号 %s", deref(ack.BrokerOrderID))
	order := waitFilled(t, b, openReq.ClientOrderID, ack.BrokerOrderID, lot)
	if order == nil {
		return
	}

	// 2. 信用建玉に現れ、建玉番号が取れるか
	rows, err := b.marginRows(symbol)
	if err != nil {
		t.Fatalf("❌ 信用建玉: %v", err)
	}
	for _, row := range rows {
		for _, k := range sortedKeys(row) {
			t.Logf("  建玉 %-32s = %q", k, text(row[k]))
		}
	}
	positions, err := b.MarginPositions()
	if err != nil {
		t.Fatalf("❌ MarginPositions: %v", err)
	}
	var pos *domain.Position
	for i := range positions {
		if positions[i].Symbol == symbol {
			pos = &positions[i]
		}
	}
	if pos == nil || pos.BrokerPositionID == "" {
		t.Fatalf("❌ MarginPositions に %s が無いか建玉番号が空: %+v", symbol, positions)
	}
	t.Logf("✅ MarginPositions: 数量 %s  返済可能 %s  建単価 %s  建玉番号 %s  取引 %s",
		pos.Quantity, pos.AvailableQuantity, pos.CostPrice, pos.BrokerPositionID, pos.Trade)

	// 3. 返済の逆指値（発火しない水準）→ 照会 → 訂正 → 取消
	stopProbe(t, b, symbol, lot, q.Bid, seed)

	// 4. 返済売り（買気配の指値）
	closed := closeLong(t, b, symbol, lot, seed+"|close")
	if closed == nil {
		return
	}

	// 5. 建玉が消えたか
	time.Sleep(2 * time.Second)
	rows, err = b.marginRows(symbol)
	if err != nil {
		t.Errorf("❌ 返済後の信用建玉: %v", err)
	} else if len(rows) > 0 {
		t.Errorf("❌ 返済後も建玉が %d 行残っている", len(rows))
	} else {
		t.Logf("✅ 返済後の信用建玉: 0 行")
	}
	t.Logf("翌営業日の単品照会の確認用: 新規 %s / 返済 %s", deref(ack.BrokerOrderID), deref(closed.BrokerOrderID))
}

// stopProbe は信用買建に返済売りの逆指値を置き、照会・訂正・取消を通す。
func stopProbe(t *testing.T, b *TachibanaBroker, symbol string, qty, bid decimal.Decimal, seed string) {
	trigger, err := marketrules.SnapToTick(bid.Mul(decimal.RequireFromString("0.97")),
		domain.SideSell, false, marketrules.RoundingConservative)
	if err != nil {
		t.Errorf("❌ 逆指値の条件: %v", err)
		return
	}
	req, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed+"|stop", symbol, domain.SideSell, qty), symbol,
		domain.SideSell, domain.OrderTypeMarket, qty, nil,
		domain.TaxAccountSpecific, "信用返済の逆指値の実機検証", domain.TradeTypeMarginClose)
	if err != nil {
		t.Error(err)
		return
	}
	if req, err = req.WithStop(trigger, nil); err != nil {
		t.Error(err)
		return
	}
	ack, err := b.Place(req)
	if err != nil {
		t.Errorf("❌ 返済の逆指値: %v", err)
		return
	}
	t.Logf("✅ 返済の逆指値: 受理 条件 %s 円 注文番号 %s", trigger, deref(ack.BrokerOrderID))
	defer func() {
		if err := b.Cancel(req.ClientOrderID, ack.BrokerOrderID); err != nil {
			t.Errorf("❌ 逆指値の取消: %v（手で取消すこと 注文番号 %s）", err, deref(ack.BrokerOrderID))
			return
		}
		time.Sleep(2 * time.Second)
		o, err := getOrder(b, req.ClientOrderID, ack.BrokerOrderID)
		if err != nil {
			t.Errorf("❌ 取消後の照会: %v", err)
			return
		}
		t.Logf("✅ 逆指値の取消: 状態 %s", o.Status)
	}()

	time.Sleep(2 * time.Second)
	o, err := getOrder(b, req.ClientOrderID, ack.BrokerOrderID)
	if err != nil {
		t.Errorf("❌ 逆指値の照会: %v", err)
		return
	}
	if o.Stop == nil {
		t.Errorf("❌ 逆指値の照会: 状態 %s だが Stop が nil", o.Status)
		return
	}
	t.Logf("✅ 逆指値の照会: 状態 %s  取引 %s  条件 %s  発火 %v", o.Status, o.Trade, o.Stop.Trigger, o.StopTriggered)

	lower, err := marketrules.SnapToTick(trigger.Mul(decimal.RequireFromString("0.98")),
		domain.SideSell, false, marketrules.RoundingConservative)
	if err != nil {
		t.Errorf("❌ 訂正の条件: %v", err)
		return
	}
	if err := b.CorrectStop(req.ClientOrderID, ack.BrokerOrderID, domain.StopSpec{Trigger: lower}); err != nil {
		t.Errorf("❌ 逆指値の訂正: %v", err)
		return
	}
	time.Sleep(2 * time.Second)
	if o, err = getOrder(b, req.ClientOrderID, ack.BrokerOrderID); err != nil || o.Stop == nil {
		t.Errorf("❌ 訂正後の照会: %v", err)
		return
	}
	t.Logf("✅ 逆指値の訂正: 条件 %s → %s（照会 %s）", trigger, lower, o.Stop.Trigger)
}

// closeLong は買建を買気配の指値で返済し、約定まで待つ。
func closeLong(t *testing.T, b *TachibanaBroker, symbol string, qty decimal.Decimal, seed string) *domain.Order {
	quote, err := b.MarketPrices([]string{symbol})
	if err != nil || !quote[symbol].Bid.IsPositive() {
		t.Errorf("❌ 返済の気配が取れません: %v（手で返済すること）", err)
		return nil
	}
	bid := quote[symbol].Bid
	req, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed, symbol, domain.SideSell, qty), symbol,
		domain.SideSell, domain.OrderTypeLimit, qty, &bid,
		domain.TaxAccountSpecific, "信用返済の実機検証", domain.TradeTypeMarginClose)
	if err != nil {
		t.Error(err)
		return nil
	}
	ack, err := b.Place(req)
	if err != nil {
		t.Errorf("❌ 信用返済: %v（手で返済すること）", err)
		return nil
	}
	t.Logf("✅ 信用返済: 受理 指値 %s 円 注文番号 %s", bid, deref(ack.BrokerOrderID))
	return waitFilled(t, b, req.ClientOrderID, ack.BrokerOrderID, qty)
}

// waitFilled は約定まで最長 30 秒待つ。約定しなければ取消して nil。
func waitFilled(t *testing.T, b *TachibanaBroker, clientOrderID string, brokerOrderID *string, qty decimal.Decimal) *domain.Order {
	for i := 0; i < 15; i++ {
		time.Sleep(2 * time.Second)
		o, err := getOrder(b, clientOrderID, brokerOrderID)
		if err != nil {
			t.Logf("  照会できません: %v", err)
			continue
		}
		if o.FilledQuantity.GreaterThanOrEqual(qty) {
			avg := "—"
			if o.AvgFillPrice != nil {
				avg = o.AvgFillPrice.String()
			}
			t.Logf("✅ 約定: 状態 %s  取引 %s  売買 %s  約定 %s 株  約定単価 %s", o.Status, o.Trade, o.Side, o.FilledQuantity, avg)
			return o
		}
		if o.Status.IsTerminal() {
			t.Errorf("❌ 約定せずに終わった: 状態 %s", o.Status)
			return nil
		}
	}
	t.Errorf("❌ 30 秒で約定しないので取消す")
	if err := b.Cancel(clientOrderID, brokerOrderID); err != nil {
		t.Errorf("❌ 取消: %v（手で取消すこと 注文番号 %s）", err, deref(brokerOrderID))
	}
	return nil
}

// getOrder は GetOrder の「該当なし（nil, nil）」をエラーにする。
func getOrder(b *TachibanaBroker, clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	o, err := b.GetOrder(clientOrderID, brokerOrderID)
	if err == nil && o == nil {
		return nil, fmt.Errorf("注文番号 %s が照会で見つからない", deref(brokerOrderID))
	}
	return o, err
}

func deref(s *string) string {
	if s == nil {
		return "—"
	}
	return *s
}
