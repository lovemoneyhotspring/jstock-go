package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/shopspring/decimal"
)

// UnconfirmedGrace は送信中（PENDING）のままブローカーに無い注文を
// 「届かなかった」と見なすまでの時間。
//
// 送った直後は照会に反映されていないことがあるので、翌日まで待つ。
const UnconfirmedGrace = 24 * time.Hour

// StatusChange は照会で分かった注文の変化。
type StatusChange struct {
	ClientOrderID  string
	Symbol         string
	Before         string
	After          domain.OrderStatus
	FilledQuantity decimal.Decimal
	Quantity       decimal.Decimal
}

// LostAmountRatio は未約定のまま終わった割合（0 なら全部約定）。
func (c StatusChange) LostAmountRatio() decimal.Decimal {
	switch c.After {
	case domain.OrderStatusCancelled, domain.OrderStatusRejected, domain.OrderStatusExpired:
	default:
		return decimal.Zero
	}
	if c.Quantity.LessThanOrEqual(decimal.Zero) {
		return decimal.NewFromInt(1)
	}
	return c.Quantity.Sub(c.FilledQuantity).Div(c.Quantity)
}

// Describe は人に読ませる 1 行。
func (c StatusChange) Describe() string {
	filled, total := c.FilledQuantity.String(), c.Quantity.String()
	lost := c.LostAmountRatio()
	if lost.IsZero() {
		return fmt.Sprintf("%s: %s（%s/%s 約定）", c.Symbol, c.After, filled, total)
	}
	pct := lost.Mul(decimal.NewFromInt(100)).Round(0)
	return fmt.Sprintf("%s: %s（%s/%s 約定、未約定 %s%% は次回に持ち越し）",
		c.Symbol, c.After, filled, total, pct)
}

// UnresolvedOrder は照会できず、判断を保留した注文。
//
// 台帳はそのまま（「発注済み」に数えたまま）にする。勝手に失効へ倒すと
// 板に残っていた注文と二重になり、逆に放置し続けると当月の予算が
// 埋まらない。どちらも自動では決められないので、人に知らせる。
type UnresolvedOrder struct {
	ClientOrderID string
	Symbol        string
	Status        string
	Reason        string
}

func (u UnresolvedOrder) Describe() string {
	return fmt.Sprintf("%s（%s / %s）: %s", u.Symbol, u.ClientOrderID, u.Status, u.Reason)
}

// SyncResult は照会の結果。
type SyncResult struct {
	// Changes は台帳を更新できた注文。
	Changes []StatusChange
	// Unresolved は照会できず保留した注文。空でなければ人に知らせる。
	Unresolved []UnresolvedOrder
}

// SyncOrderStatus は結果が確定していない注文をブローカーに照会し、台帳を更新する。
//
// ブローカーに無い注文は原則そのまま残す（勝手に「失効」にすると、実は板に
// 残っていた注文と二重になる）。例外は**送信中（PENDING）のまま
// UnconfirmedGrace を過ぎても無い**注文——応答が返らず記録だけが残ったもので、
// 届いていれば翌日には照会できる。これは REJECTED に落とし、次の実行で
// 差額として埋め直す。これをしないと、届かなかった注文が永久に「発注済み」
// として当月の予算を食い続ける。
//
// 約定単価が分かった注文は「発注済み」の額を **株数 × 約定単価** に置き換える。
// 判断時の価格のままだと、実際に払った額との差だけ差額の計算がずれる。
// 約定ぶんではなく発注株数を掛けるのは、失効・拒否のときに
// ledger.EffectiveAmount が約定ぶんへ按分するため——ここで按分すると二重に効く。
func SyncOrderStatus(led *ledger.Ledger, b broker.Broker, now time.Time) (SyncResult, error) {
	var result SyncResult
	open, err := led.OpenOrders()
	if err != nil {
		return result, err
	}

	for _, row := range open {
		// broker_order_id は client_order_id で引けないブローカー（立花証券）のためのヒント。
		// 送信結果が分からず PENDING のまま残った注文にはこれが無く、照会しようが無い。
		order, err := b.GetOrder(row.ClientOrderID, row.BrokerOrderID)
		if err != nil {
			// 1 件引けないだけで残りの照会まで止めない。保留にして先へ進む
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol,
				Status: row.Status, Reason: err.Error(),
			})
			continue
		}

		if order == nil {
			if row.Status != string(domain.OrderStatusPending) {
				continue
			}
			placedAt, perr := time.Parse(time.RFC3339, row.PlacedAt)
			if perr != nil || now.Sub(placedAt) < UnconfirmedGrace {
				continue
			}
			if err := led.UpdateStatus(row.ClientOrderID,
				string(domain.OrderStatusRejected), nil, nil); err != nil {
				return result, err
			}
			execution.Collect(execution.Spec{
				Event:         "fill",
				App:           "accum",
				Symbol:        row.Symbol,
				Side:          string(domain.SideBuy),
				ClientOrderID: row.ClientOrderID,
				Live:          true,
				Quantity:      row.Quantity,
				Reason:        execution.ReasonUnconfirmed,
			})
			result.Changes = append(result.Changes, StatusChange{
				ClientOrderID:  row.ClientOrderID,
				Symbol:         row.Symbol,
				Before:         row.Status,
				After:          domain.OrderStatusRejected,
				FilledQuantity: decimal.Zero,
				Quantity:       row.Quantity,
			})
			continue
		}

		if string(order.Status) == row.Status && order.FilledQuantity.Equal(row.FilledQuantity) {
			continue
		}

		// amount はこの直後に約定額で上書きされる。想定はここでしか取れない
		var intentPrice any
		if row.Amount != nil && row.Quantity.IsPositive() {
			intentPrice = row.Amount.Div(row.Quantity)
		}
		reason := execution.ReasonExpired
		if order.FilledQuantity.IsPositive() {
			reason = execution.ReasonFilled
		}
		var brokerOrderID string
		if order.BrokerOrderID != nil {
			brokerOrderID = *order.BrokerOrderID
		}
		var fillPrice any
		if order.AvgFillPrice != nil {
			fillPrice = *order.AvgFillPrice
		}
		execution.Collect(execution.Spec{
			Event:         "fill",
			App:           "accum",
			Symbol:        row.Symbol,
			Side:          string(domain.SideBuy),
			ClientOrderID: row.ClientOrderID,
			BrokerOrderID: brokerOrderID,
			Live:          true,
			Quantity:      row.Quantity,
			IntentPrice:   intentPrice,
			FillQuantity:  order.FilledQuantity,
			FillPrice:     fillPrice,
			Reason:        reason,
		})

		var amount *decimal.Decimal
		if order.AvgFillPrice != nil {
			a := row.Quantity.Mul(*order.AvgFillPrice)
			amount = &a
		}
		if err := led.UpdateStatusDetail(row.ClientOrderID, string(order.Status),
			&order.FilledQuantity, order.AvgFillPrice, order.BrokerOrderID, amount); err != nil {
			return result, err
		}
		result.Changes = append(result.Changes, StatusChange{
			ClientOrderID:  row.ClientOrderID,
			Symbol:         row.Symbol,
			Before:         row.Status,
			After:          order.Status,
			FilledQuantity: order.FilledQuantity,
			Quantity:       row.Quantity,
		})
	}
	return result, nil
}
