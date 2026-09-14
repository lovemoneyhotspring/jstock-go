package broker

// 信用の発注経路を実機で 1 周させる調べもの（docs/BROKER_VERIFY.md の信用 4 点と手順 5 e）。
// **実際に発注する。** 1 単元を信用で建て、同じ実行の中で返済まで済ませる。
//
//	WBJP_ENV=prod WBJP_ENV_FILE=$PWD/.env \
//	  TACHIBANA_PROD_PRIVATE_KEY_FILE=$PWD/e_api_private_key.der \
//	  TACHIBANA_MARGIN_ORDER_PROBE=2012 go test ./pkg/wbcore/broker -run TestMarginOrderProbe -v -count=1
//
// TACHIBANA_MARGIN_ORDER_SIDE=sell で売建（空売り → 返済買い）、
// TACHIBANA_MARGIN_ORDER_TYPE=market で新規・返済とも成行（daytrade の open / close と同じ）。
//
// 安全のための約束:
//   - 1 単元・見積り 5,000 円まで（TACHIBANA_MARGIN_ORDER_MAX_YEN で変える）
//   - その銘柄に信用建玉が既にあれば何も送らない（他の玉を返済しないため）
//   - 指値のときは約定する側の気配に置く（買いは売気配、売りは買気配）
//   - 返済の逆指値は発火しない水準（買建は買気配 −3%、売建は売気配 +3%）に置き、訂正して取消す
//   - 途中で落ちても、建った玉は最後に必ず返済を試みる
import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

// marginProbe は 1 回の検証の指定。
type marginProbe struct {
	b      *TachibanaBroker
	symbol string
	lot    decimal.Decimal
	// open は新規の売買（買建なら Buy）。返済はその反対
	open   domain.Side
	market bool
}

func (p marginProbe) closeSide() domain.Side {
	if p.open == domain.SideBuy {
		return domain.SideSell
	}
	return domain.SideBuy
}

// limitFor は約定する側の気配（買いは売気配、売りは買気配）。成行なら nil。
func (p marginProbe) limitFor(side domain.Side, q MarketPrice) *decimal.Decimal {
	if p.market {
		return nil
	}
	price := q.Bid
	if side == domain.SideBuy {
		price = q.Ask
	}
	return &price
}

func (p marginProbe) orderType() domain.OrderType {
	if p.market {
		return domain.OrderTypeMarket
	}
	return domain.OrderTypeLimit
}

func newProbeBroker(t *testing.T) *TachibanaBroker {
	app := settings.LoadAppSettings()
	creds, err := credentials.LoadTachibanaCredentials(app.Env, app.DotenvMap)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTachibanaBroker(app.Env, creds, app.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

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
	p := marginProbe{b: newProbeBroker(t), symbol: symbol, open: domain.SideBuy}
	switch side := os.Getenv("TACHIBANA_MARGIN_ORDER_SIDE"); side {
	case "", "buy":
	case "sell":
		p.open = domain.SideSell
	default:
		t.Fatalf("TACHIBANA_MARGIN_ORDER_SIDE は buy か sell: %q", side)
	}
	switch typ := os.Getenv("TACHIBANA_MARGIN_ORDER_TYPE"); typ {
	case "", "limit":
	case "market":
		p.market = true
	default:
		t.Fatalf("TACHIBANA_MARGIN_ORDER_TYPE は limit か market: %q", typ)
	}
	b := p.b

	// 0. 売買単位・気配・既存の建玉
	p.lot = b.LotSizes([]string{symbol})[symbol]
	if !p.lot.IsPositive() {
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
	estimate := p.lot.Mul(q.Ask)
	t.Logf("%s 単元 %s  買気配 %s  売気配 %s  見積り %s 円（上限 %s 円）  新規 %s  %s",
		symbol, p.lot, q.Bid, q.Ask, estimate, maxYen, p.open, p.orderType())
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
		p.closePosition(t, seed+"|cleanup")
	}()

	// 1. 信用新規
	openReq, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed+"|open", symbol, p.open, p.lot), symbol,
		p.open, p.orderType(), p.lot, p.limitFor(p.open, q),
		domain.TaxAccountSpecific, "信用新規の実機検証", domain.TradeTypeMarginOpen)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := b.Place(openReq)
	if err != nil {
		t.Fatalf("❌ 信用新規: %v", err)
	}
	opened = true
	t.Logf("✅ 信用新規: 受理 %s %s 注文番号 %s", p.open, priceText(openReq.LimitPrice), deref(ack.BrokerOrderID))
	if waitFilled(t, b, openReq.ClientOrderID, ack.BrokerOrderID, p.lot) == nil {
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
	// 売建は数量を負で返す約束
	wantNegative := p.open == domain.SideSell
	if pos.Quantity.IsNegative() != wantNegative {
		t.Errorf("❌ MarginPositions の数量の符号が違う: %s（新規 %s）", pos.Quantity, p.open)
	}
	t.Logf("✅ MarginPositions: 数量 %s  返済可能 %s  建単価 %s  建玉番号 %s  取引 %s",
		pos.Quantity, pos.AvailableQuantity, pos.CostPrice, pos.BrokerPositionID, pos.Trade)

	// 3. 返済の逆指値（発火しない水準）→ 照会 → 訂正 → 取消
	p.stopProbe(t, q, seed)

	// 4. 返済
	closed := p.closePosition(t, seed+"|close")
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

// stopProbe は建玉に返済の逆指値を置き、照会・訂正・取消を通す。
//
// 買建の返済売りは「条件以下で発火」なので気配の下、売建の返済買いは「条件以上で発火」
// なので気配の上に置く。訂正はさらに遠ざける。
func (p marginProbe) stopProbe(t *testing.T, q MarketPrice, seed string) {
	b, side := p.b, p.closeSide()
	base, away, further := q.Bid, "0.97", "0.98"
	if side == domain.SideBuy {
		base, away, further = q.Ask, "1.03", "1.02"
	}
	trigger, err := marketrules.SnapToTick(base.Mul(decimal.RequireFromString(away)),
		side, false, marketrules.RoundingConservative)
	if err != nil {
		t.Errorf("❌ 逆指値の条件: %v", err)
		return
	}
	req, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed+"|stop", p.symbol, side, p.lot), p.symbol,
		side, domain.OrderTypeMarket, p.lot, nil,
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
	t.Logf("✅ 返済の逆指値: 受理 %s 条件 %s 円 注文番号 %s", side, trigger, deref(ack.BrokerOrderID))
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
	if o.StopTriggered {
		t.Errorf("❌ 逆指値が発火している（発火しない水準のはず）: 条件 %s", o.Stop.Trigger)
		return
	}
	t.Logf("✅ 逆指値の照会: 状態 %s  取引 %s  売買 %s  条件 %s  発火 %v", o.Status, o.Trade, o.Side, o.Stop.Trigger, o.StopTriggered)

	moved, err := marketrules.SnapToTick(trigger.Mul(decimal.RequireFromString(further)),
		side, false, marketrules.RoundingConservative)
	if err != nil {
		t.Errorf("❌ 訂正の条件: %v", err)
		return
	}
	if err := b.CorrectStop(req.ClientOrderID, ack.BrokerOrderID, domain.StopSpec{Trigger: moved}); err != nil {
		t.Errorf("❌ 逆指値の訂正: %v", err)
		return
	}
	time.Sleep(2 * time.Second)
	if o, err = getOrder(b, req.ClientOrderID, ack.BrokerOrderID); err != nil || o.Stop == nil {
		t.Errorf("❌ 訂正後の照会: %v", err)
		return
	}
	t.Logf("✅ 逆指値の訂正: 条件 %s → %s（照会 %s）", trigger, moved, o.Stop.Trigger)
}

// closePosition は建玉を返済し、約定まで待つ。指値なら約定する側の気配に置く。
func (p marginProbe) closePosition(t *testing.T, seed string) *domain.Order {
	b, side := p.b, p.closeSide()
	quote, err := b.MarketPrices([]string{p.symbol})
	if err != nil || !quote[p.symbol].Bid.IsPositive() || !quote[p.symbol].Ask.IsPositive() {
		t.Errorf("❌ 返済の気配が取れません: %v（手で返済すること）", err)
		return nil
	}
	req, err := domain.NewOrderRequest(
		domain.MakeClientOrderID(seed, p.symbol, side, p.lot), p.symbol,
		side, p.orderType(), p.lot, p.limitFor(side, quote[p.symbol]),
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
	t.Logf("✅ 信用返済: 受理 %s %s 注文番号 %s", side, priceText(req.LimitPrice), deref(ack.BrokerOrderID))
	return waitFilled(t, b, req.ClientOrderID, ack.BrokerOrderID, p.lot)
}

// TestOrderDetailProbe は前営業日以前の注文を単品照会（CLMOrderListDetail）で引けるかを見る
// （BROKER_VERIFY の信用 4 点目）。照会だけで発注はしない。
//
//	TACHIBANA_ORDER_DETAIL_PROBE=14012403/20260914,14012415/20260914 go test ./pkg/wbcore/broker -run TestOrderDetailProbe -v -count=1
func TestOrderDetailProbe(t *testing.T) {
	ids := os.Getenv("TACHIBANA_ORDER_DETAIL_PROBE")
	if ids == "" {
		t.Skip("TACHIBANA_ORDER_DETAIL_PROBE=<注文番号/営業日>[,...] を立てたときだけ動かす")
	}
	b := newProbeBroker(t)
	for _, id := range strings.Split(ids, ",") {
		id = strings.TrimSpace(id)
		o, err := b.GetOrder("", &id)
		switch {
		case err != nil:
			t.Errorf("❌ %s: %v", id, err)
		case o == nil:
			t.Errorf("❌ %s: 該当なし（前営業日の注文は単品照会でも返らない）", id)
		default:
			avg := "—"
			if o.AvgFillPrice != nil {
				avg = o.AvgFillPrice.String()
			}
			t.Logf("✅ %s: %s %s %s  状態 %s  数量 %s  約定 %s  約定単価 %s",
				id, o.Symbol, o.Trade, o.Side, o.Status, o.Quantity, o.FilledQuantity, avg)
		}
	}
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
			t.Logf("✅ 約定: 状態 %s  取引 %s  売買 %s  種別 %s  約定 %s 株  約定単価 %s",
				o.Status, o.Trade, o.Side, o.OrderType, o.FilledQuantity, avg)
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

func priceText(p *decimal.Decimal) string {
	if p == nil {
		return "成行"
	}
	return "指値 " + p.String() + " 円"
}

func deref(s *string) string {
	if s == nil {
		return "—"
	}
	return *s
}
