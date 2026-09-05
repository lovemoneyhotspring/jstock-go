package broker

import (
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 立花証券の注文照会。項目名は削除済み Python 実装（git show
// ac1eb7a:src/wbcore/broker/tachibana.py）と API リファレンスに合わせてある。
//
// 電文ごとに同じ内容でも項目名が違う。注文一覧（CLMOrderList）は sOrder* の
// 接頭辞が付き、単品照会（CLMOrderListDetail）は付かない——ここを取り違えると
// 約定数量が 0 として読めてしまうので、電文ごとに別の定数として持つ。

// 注文一覧（CLMOrderList）の項目。
const (
	orderListKey       = "aOrderList"
	fieldListNumber    = "sOrderOrderNumber"
	fieldListDay       = "sOrderSikkouDay"
	fieldListIssue     = "sOrderIssueCode"
	fieldListQty       = "sOrderOrderSuryou"
	fieldListFilledQty = "sOrderYakuzyouSuryo"
	fieldListFillPrice = "sOrderYakuzyouPrice"
)

// 単品照会（CLMOrderListDetail）の項目。
const (
	fieldDetailIssue     = "sIssueCode"
	fieldDetailFilledQty = "sYakuzyouSuryou"
	fieldDetailFillPrice = "sYakuzyouPrice"
)

// 一覧・単品で共通の項目。
const (
	fieldOrderStatusCode = "sOrderStatusCode"
	fieldSideKubun       = "sOrderBaibaiKubun"
	fieldTradeKubun      = "sGenkinSinyouKubun" // ※ Shinyou ではなく Sinyou
	fieldPriceKubun      = "sOrderOrderPriceKubun"
	fieldOrderPrice      = "sOrderOrderPrice"
	fieldOrderDateTime   = "sOrderOrderDateTime"
)

// openOrderStatusFilter は「未約定＋一部約定」だけを引く sOrderSyoukaiStatus。
const openOrderStatusFilter = "5"

// brokerOrderIDOf は注文番号と営業日から broker_order_id（"番号/営業日"）を作る。
func brokerOrderIDOf(orderNumber, orderDay string) string {
	return fmt.Sprintf("%s/%s", strings.TrimSpace(orderNumber), strings.TrimSpace(orderDay))
}

// splitBrokerOrderID は "番号/営業日" を分解する。
func splitBrokerOrderID(value string) (number, day string, ok bool) {
	number, day, ok = strings.Cut(strings.TrimSpace(value), "/")
	if !ok || number == "" || day == "" {
		return "", "", false
	}
	return number, day, true
}

// orderFields は電文ごとに違う項目名の組。
type orderFields struct {
	issue     string
	quantity  string
	filledQty string
	fillPrice string
	// 逆指値の項目。一覧は sOrder* の接頭辞付き、単品照会は無し
	stopType    string
	stopTrigger string
	stopKubun   string
	stopPrice   string
	triggerType string
}

var (
	listFields = orderFields{
		issue: fieldListIssue, quantity: fieldListQty,
		filledQty: fieldListFilledQty, fillPrice: fieldListFillPrice,
		stopType: "sOrderGyakusasiOrderType", stopTrigger: "sOrderGyakusasiZyouken",
		stopKubun: "sOrderGyakusasiKubun", stopPrice: "sOrderGyakusasiPrice",
		triggerType: "sOrderTriggerType",
	}
	detailFields = orderFields{
		issue: fieldDetailIssue, quantity: fieldListQty,
		filledQty: fieldDetailFilledQty, fillPrice: fieldDetailFillPrice,
		stopType: "sGyakusasiOrderType", stopTrigger: "sGyakusasiZyouken",
		stopKubun: "sGyakusasiKubun", stopPrice: "sGyakusasiPrice",
		triggerType: "sTriggerType",
	}
)

// toOrder は応答 1 行を domain.Order にする。
//
// 売買区分・現金信用区分が解釈できない行はエラー。現渡・現引や未知の区分を
// 買い・現物として通すと、手仕舞いの向きや数量を間違える。
func toOrder(row map[string]any, f orderFields, number, day, clientOrderID string) (domain.Order, error) {
	side, err := ParseSide(field(row, fieldSideKubun))
	if err != nil {
		return domain.Order{}, err
	}
	trade, err := ParseTrade(field(row, fieldTradeKubun))
	if err != nil {
		return domain.Order{}, err
	}

	brokerID := brokerOrderIDOf(number, day)
	if clientOrderID == "" {
		clientOrderID = brokerID
	}

	priceKubun := field(row, fieldPriceKubun)
	orderType, ok := orderTypeFromCode[priceKubun]
	if !ok {
		orderType = domain.OrderTypeOther
	}

	order := domain.Order{
		ClientOrderID:  clientOrderID,
		BrokerOrderID:  &brokerID,
		Symbol:         field(row, f.issue),
		Side:           side,
		OrderType:      orderType,
		Quantity:       fieldDecimal(row, f.quantity),
		FilledQuantity: fieldDecimal(row, f.filledQty),
		Status:         ParseStatus(field(row, fieldOrderStatusCode)),
		Trade:          trade,
	}
	// 指値は「指値の注文種別」のときだけ意味がある（成行は 0 が入る）
	if limit := fieldDecimal(row, fieldOrderPrice); limit.IsPositive() &&
		(priceKubun == "2" || priceKubun == "4") {
		order.LimitPrice = &limit
	}
	if price := fieldDecimal(row, f.fillPrice); price.IsPositive() {
		order.AvgFillPrice = &price
	}
	if at, ok := parseJSTDateTime(field(row, fieldOrderDateTime)); ok {
		order.CreatedAt = &at
	}
	// 逆指値（1）・通常＋逆指値（2）。値段区分 1 が指値、0 が成行。
	// トリガータイプは 0 が未トリガーで、それ以外は発火済み
	if stopType := field(row, f.stopType); stopType == "1" || stopType == "2" {
		spec := domain.StopSpec{Trigger: fieldDecimal(row, f.stopTrigger)}
		if field(row, f.stopKubun) == "1" {
			if price := fieldDecimal(row, f.stopPrice); price.IsPositive() {
				spec.Price = &price
			}
		}
		order.Stop = &spec
		trigger := field(row, f.triggerType)
		order.StopTriggered = trigger != "" && trigger != "0"
	}
	return order, nil
}

// orderList は CLMOrderList を引く。status は sOrderSyoukaiStatus（空で全件）。
func (t *TachibanaBroker) orderList(status string) ([]domain.Order, error) {
	res, err := t.postRequest(clmOrderList, map[string]any{
		"sIssueCode":          "",
		"sSikkouDay":          "",
		"sOrderSyoukaiStatus": status,
	})
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmOrderList); err != nil {
		return nil, err
	}
	rows, err := rowsOf(res, orderListKey, clmOrderList)
	if err != nil {
		return nil, err
	}

	orders := make([]domain.Order, 0, len(rows))
	for _, row := range rows {
		number := field(row, fieldListNumber)
		if number == "" {
			continue // 注文番号が無い行は以後照会できないので注文として扱わない
		}
		order, err := toOrder(row, listFields, number, field(row, fieldListDay),
			t.clientOrderIDFor(number))
		if err != nil {
			// 1 行読めないだけで一覧全体を落とさない。ただし黙らせない
			t.logWarn("broker.order_row_unreadable", "注文一覧の 1 行を解釈できません",
				map[string]any{"order_number": number, "error": err.Error()})
			continue
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// GetOpenOrders は板に残っている注文（未約定＋一部約定）。
//
// 立花の注文一覧は**当日（＋繰越）分しか返らない**。
func (t *TachibanaBroker) GetOpenOrders() ([]domain.Order, error) {
	return t.orderList(openOrderStatusFilter)
}

// GetOrderHistory は期間内の注文。
//
// **当日（＋繰越）分しか返らない**（CLMOrderList の制約。過去日は取れない）。
// 積立の「台帳に無い当月の約定」の突合に使われる。当日の再実行（台帳を消した
// 直後など）は捕まえられるが、前日以前の約定は見えないので警告を残す。
func (t *TachibanaBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	today := t.today()
	startDay := start.In(jst()).Format("2006-01-02")
	todayDay := today.Format("2006-01-02")
	if startDay < todayDay {
		t.logWarn("broker.history_today_only", "立花証券の注文一覧は当日分のみ。前日以前の注文は突合できません",
			map[string]any{"start": startDay, "today": todayDay})
	}
	if end.In(jst()).Format("2006-01-02") < todayDay {
		return nil, nil
	}
	return t.orderList("")
}

// GetOrder は 1 件の注文の現在の状態を返す。
//
// 立花は client_order_id を持たないので、照会には注文番号（broker_order_id、
// "注文番号/営業日"）が要る。分からないときは**エラー**にする——nil を返すと
// 呼び出し側が「ブローカーに無い＝再送してよい」と誤認する。
func (t *TachibanaBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	var raw string
	if brokerOrderID != nil {
		raw = *brokerOrderID
	}
	if raw == "" {
		raw = t.nativeOrderID(clientOrderID)
	}
	number, day, ok := splitBrokerOrderID(raw)
	if !ok {
		return nil, fmt.Errorf(
			"client_order_id=%q の立花証券の注文番号が分かりません"+
				"（台帳の broker_order_id を渡してください）", clientOrderID)
	}

	res, err := t.postRequest(clmOrderDetail, map[string]any{
		"sOrderNumber": number,
		"sEigyouDay":   day,
	})
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmOrderDetail); err != nil {
		// 該当が無いことを業務エラーで返す実装があるので、その場合だけ nil
		if isOrderNotFound(res) {
			return nil, nil
		}
		return nil, err
	}

	order, err := toOrder(res, detailFields, number, day, clientOrderID)
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// isOrderNotFound は「その注文は無い」を表す応答か。
//
// 通信や項目名の誤りと区別するために、結果コードで判定する。
func isOrderNotFound(res map[string]any) bool {
	code := strings.TrimSpace(text(res["sResultCode"]))
	return code == orderNotFoundCode
}

// LotSizes は売買単位（CLMStkGetIssueMstKabu の sBaibaiTani）。
//
// マスタは全銘柄が一括で返るので 1 プロセスに 1 回だけ取って使い回す。
// 取れなかった銘柄はキーごと省く（呼び出し側が既定や設定で補う）。
func (t *TachibanaBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(symbols))
	master, err := t.lotMaster()
	if err != nil {
		t.logWarn("broker.lot_master_failed", "売買単位のマスタを取得できません", map[string]any{"error": err.Error()})
		return out
	}
	for _, symbol := range symbols {
		if unit, ok := master[symbol]; ok && unit.IsPositive() {
			out[symbol] = unit
		}
	}
	return out
}

func (t *TachibanaBroker) lotMaster() (map[string]decimal.Decimal, error) {
	t.masterMu.Lock()
	defer t.masterMu.Unlock()
	if t.lotSizeMaster != nil {
		return t.lotSizeMaster, nil
	}

	res, err := t.postTo(interfaceMaster, clmStockMaster, map[string]any{})
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmStockMaster); err != nil {
		return nil, err
	}
	rows, err := rowsOf(res, stockMasterKey, clmStockMaster)
	if err != nil {
		return nil, err
	}

	master := make(map[string]decimal.Decimal, len(rows))
	for _, row := range rows {
		code := field(row, "sIssueCode")
		if code == "" {
			continue
		}
		if unit := fieldDecimal(row, "sBaibaiTani"); unit.IsPositive() {
			master[code] = unit
		}
	}
	t.lotSizeMaster = master
	return master, nil
}

// parseJSTDateTime は立花の日時（YYYYMMDDHHMMSS 等）を JST の時刻にする。
func parseJSTDateTime(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{"20060102150405", "2006/01/02 15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, jst()); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}
