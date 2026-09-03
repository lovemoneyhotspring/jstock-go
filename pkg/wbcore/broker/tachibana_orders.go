package broker

import (
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 立花証券の注文照会（CLMOrderList）。
//
// **実機未検証。** 本番・デモとも API を叩けていないため、電文の項目名は
// 資料と既存の電文（CLMGenbutuKabuList 等）の命名から起こした推定を含む。
// 検証で違いが見つかったら、直すのはこのファイルの定数だけで済むように
// 名前を 1 箇所に集めてある。応答の形が想定と違えば
// ErrUnverifiedResponse で必ず落ちる（空の結果として通さない）。

// 注文照会の応答の項目名。実機で確認したら**ここだけ**直す。
const (
	// orderListKey は注文一覧が入る配列のキー。
	orderListKey = "aCLMOrderList"

	// 注文 1 件の項目。候補を並べてあるものは、電文により名前が揺れるもの。
	fieldOrderNumber = "sOrderNumber"
	fieldOrderDay    = "sEigyouDay"
	fieldIssueCode   = "sIssueCode"
	fieldOrderStatus = "sOrderStatus"
	fieldSideKubun   = "sOrderBaibaiKubun"
	fieldTradeKubun  = "sGenkinShinyouKubun"
)

// 数量・価格は電文ごとに名前が揺れるので候補を並べる。
var (
	fieldsOrderQty   = []string{"sOrderSuryo", "sOrderOrderSuryo", "sOrderCount"}
	fieldsFilledQty  = []string{"sOrderYakuzyouSuryo", "sYakuzyouSuryo", "sOrderYakuzyouCount"}
	fieldsFillPrice  = []string{"sOrderYakuzyouTanka", "sYakuzyouTanka", "sOrderYakuzyouPrice"}
	fieldsLimitPrice = []string{"sOrderPrice", "sOrderOrderPrice"}
)

// tachibanaOrderStatus は立花の注文状態コードを domain の状態に写す。
//
// 表に無いコードは OrderStatusUnknown。Unknown は IsTerminal が false
// （＝まだ生きている扱い）なので、知らないコードを「終わった」と誤認して
// 台帳から落とすことはない。安全側に倒れる向きを選んである。
var tachibanaOrderStatus = map[string]domain.OrderStatus{
	"0": domain.OrderStatusPending,   // 未処理
	"1": domain.OrderStatusSubmitted, // 受付済
	"2": domain.OrderStatusSubmitted, // 発注済（板にある）
	"3": domain.OrderStatusPartiallyFilled,
	"4": domain.OrderStatusFilled,
	"5": domain.OrderStatusCancelled,
	"6": domain.OrderStatusRejected,
	"7": domain.OrderStatusExpired, // 失効
}

// ParseTachibanaOrderStatus は状態コードを domain の状態にする。
// 約定数量と突き合わせるのは呼び出し側の仕事。
func ParseTachibanaOrderStatus(code string) domain.OrderStatus {
	if status, ok := tachibanaOrderStatus[strings.TrimSpace(code)]; ok {
		return status
	}
	return domain.OrderStatusUnknown
}

// brokerOrderIDOf は注文番号と営業日から broker_order_id（"番号/営業日"）を作る。
// Place が同じ形で返すので、台帳の値と突き合わせられる。
func brokerOrderIDOf(orderNumber, orderDay string) string {
	return fmt.Sprintf("%s/%s", strings.TrimSpace(orderNumber), strings.TrimSpace(orderDay))
}

// GetOrderHistory は期間内の注文を返す。
//
// start / end は JST の日付として渡す（時刻は無視される）。
// 応答の形が想定と違えばエラー——空の履歴として返すと、accum の
// 二重買付ガード（UnrecordedFills）が素通りする。
func (t *TachibanaBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	params := map[string]any{
		"sIssueCode":         "",
		"sSizyouC":           "",
		"sBaibaiKubun":       "",
		"sOrderSyoukaiZyouS": "", // 状態で絞らない（全部取って呼び出し側で判断する）
		"sOrderKaiInSizyouC": "",
		"sOrderSyoukaiFrom":  start.Format("20060102"),
		"sOrderSyoukaiTo":    end.Format("20060102"),
	}
	res, err := t.postRequest(clmOrderList, params)
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
		order, ok := tachibanaOrderFrom(row)
		if !ok {
			continue
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// tachibanaOrderFrom は注文 1 行を domain.Order にする。
// 注文番号が取れない行は注文として扱えないので落とす。
func tachibanaOrderFrom(row map[string]any) (domain.Order, bool) {
	number := field(row, fieldOrderNumber)
	if number == "" {
		return domain.Order{}, false
	}
	brokerID := brokerOrderIDOf(number, field(row, fieldOrderDay))

	side := domain.SideBuy
	if field(row, fieldSideKubun) == tachibanaSideSell {
		side = domain.SideSell
	}

	quantity := fieldDecimal(row, fieldsOrderQty...)
	filled := fieldDecimal(row, fieldsFilledQty...)
	status := ParseTachibanaOrderStatus(field(row, fieldOrderStatus))

	order := domain.Order{
		ClientOrderID:  "", // 立花は client_order_id を保持しない。突き合わせは broker_order_id
		BrokerOrderID:  &brokerID,
		Symbol:         field(row, fieldIssueCode),
		Side:           side,
		Quantity:       quantity,
		FilledQuantity: filled,
		Status:         status,
		Trade:          tradeTypeFromKubun(field(row, fieldTradeKubun), side),
	}
	if price, ok := fieldDecimalOK(row, fieldsFillPrice...); ok && price.IsPositive() {
		order.AvgFillPrice = &price
	}
	if limit, ok := fieldDecimalOK(row, fieldsLimitPrice...); ok && limit.IsPositive() {
		order.LimitPrice = &limit
		order.OrderType = domain.OrderTypeLimit
	} else {
		order.OrderType = domain.OrderTypeMarket
	}
	return order, true
}

// GetOpenOrders は板に残っている注文。
//
// 立花の注文照会は当日分が基本なので、当日で引いて終了していないものを返す。
func (t *TachibanaBroker) GetOpenOrders() ([]domain.Order, error) {
	today := t.today()
	orders, err := t.GetOrderHistory(today, today)
	if err != nil {
		return nil, err
	}
	open := make([]domain.Order, 0, len(orders))
	for _, o := range orders {
		if o.Status.IsOpen() {
			open = append(open, o)
		}
	}
	return open, nil
}

// GetOrder は 1 件の注文の現在の状態を返す。
//
// 立花は client_order_id を持たないので、突き合わせは broker_order_id
// （Place が返した "注文番号/営業日"）で行う。broker_order_id が無ければ
// 引きようがないのでエラーにする——nil を返すと呼び出し側が
// 「ブローカーに無い注文」と解釈し、失効や再発注に進んでしまう。
func (t *TachibanaBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	if brokerOrderID == nil || strings.TrimSpace(*brokerOrderID) == "" {
		return nil, fmt.Errorf(
			"立花証券の注文照会には broker_order_id が必要です（client_order_id=%s）", clientOrderID)
	}
	wanted := strings.TrimSpace(*brokerOrderID)

	// 営業日が分かるなら、その日だけを引く（当日以外の注文も追える）
	day := t.today()
	if parts := strings.Split(wanted, "/"); len(parts) == 2 && len(parts[1]) == 8 {
		if parsed, err := time.ParseInLocation("20060102", parts[1], jst()); err == nil {
			day = parsed
		}
	}

	orders, err := t.GetOrderHistory(day, day)
	if err != nil {
		return nil, err
	}
	for _, o := range orders {
		if o.BrokerOrderID != nil && *o.BrokerOrderID == wanted {
			found := o
			found.ClientOrderID = clientOrderID
			return &found, nil
		}
	}
	// 照会は成功したが、その注文は無かった。呼び出し側がこれを
	// 「届かなかった注文」として扱えるよう、エラーではなく nil を返す。
	return nil, nil
}

// LotSizes は銘柄ごとの売買単位を返す。
//
// 取れなかった銘柄はキーごと省く。呼び出し側は既定（100 株）や
// 設定の lot_size_overrides で補う。
func (t *TachibanaBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	out := make(map[string]decimal.Decimal, len(symbols))
	for _, symbol := range symbols {
		res, err := t.postRequest(clmStockMaster, map[string]any{"sIssueCode": symbol})
		if err != nil || checkResult(res, clmStockMaster) != nil {
			continue
		}
		if unit := fieldDecimal(res, "sBaibaiTanni", "sUriBaibaiTanni"); unit.IsPositive() {
			out[symbol] = unit
		}
	}
	return out
}
