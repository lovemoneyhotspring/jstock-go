package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/reconcile"
	"github.com/shopspring/decimal"
)

// UnconfirmedGrace は送信中（PENDING）の注文を当日の注文一覧と突き合わせるまでの猶予。
// 受付が一覧に載るまでの揺れを吸収する。run は 20 分おきなので、次の run で判定される。
const UnconfirmedGrace = 5 * time.Minute

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
	case domain.OrderStatusCancelled, domain.OrderStatusRejected, domain.OrderStatusExpired, domain.OrderStatusUnsent:
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
// 埋まらない。次の run がもう一度判定する。それでも決まらないものは
// ダイジェストの異常として残り、AI が口座の注文一覧と突き合わせる。
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
	// Unresolved は照会できず保留した注文。空でなければ知らせる（次の run で再判定）。
	Unresolved []UnresolvedOrder
	// Resolved は送信結果不明（PENDING）の注文を当日の注文一覧で判定した集計。
	Resolved reconcile.Summary
	// Resolutions はその 1 件ごとの判定（ログに項目付きで残す）。
	Resolutions []reconcile.Resolution
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

	// 送信結果が分からず注文番号の無い PENDING は、client_order_id では引けない
	// （立花は保持しない）。当日の注文一覧と銘柄・数量・時刻で突き合わせて決める
	var unconfirmed []ledger.LedgerOrder
	for _, row := range open {
		// broker_order_id は client_order_id で引けないブローカー（立花証券）のためのヒント。
		// 送信結果が分からず PENDING のまま残った注文にはこれが無い。client_order_id で
		// 引けるブローカー（paper）ならそのまま使い、引けなければ一覧の突き合わせに回す
		order, err := b.GetOrder(row.ClientOrderID, row.BrokerOrderID)
		if row.Status == string(domain.OrderStatusPending) && row.BrokerOrderID == nil && (err != nil || order == nil) {
			unconfirmed = append(unconfirmed, row)
			continue
		}
		if err != nil {
			// 1 件引けないだけで残りの照会まで止めない。保留にして先へ進む
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol,
				Status: row.Status, Reason: err.Error(),
			})
			continue
		}

		if order == nil {
			// 注文番号があるのにブローカーが知らない。勝手に失効へ倒さず保留にする
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol,
				Status: row.Status, Reason: "注文番号 " + *row.BrokerOrderID + " がブローカーの応答に無い",
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
	if len(unconfirmed) > 0 {
		if err := resolveUnconfirmed(led, b, unconfirmed, now, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

// resolveUnconfirmed は注文番号の無い PENDING を当日の注文一覧で判定し、台帳を更新する。
//
// 一覧を照会できなければ全件を保留にする（次の run で再判定）。
func resolveUnconfirmed(led *ledger.Ledger, b broker.Broker, rows []ledger.LedgerOrder, now time.Time, result *SyncResult) error {
	hold := func(reason string) {
		for _, row := range rows {
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Status: row.Status, Reason: reason,
			})
		}
	}
	jst := clock.ToZone(now, clock.Tokyo)
	start := time.Date(jst.Year(), jst.Month(), jst.Day(), 0, 0, 0, 0, clock.Tokyo)
	todays, err := b.GetOrderHistory(start, jst)
	if err != nil {
		hold("当日の注文一覧を照会できない: " + err.Error())
		return nil
	}
	known, err := led.BrokerOrderIDs()
	if err != nil {
		return err
	}
	pendings := make([]reconcile.Pending, 0, len(rows))
	byID := map[string]ledger.LedgerOrder{}
	for _, row := range rows {
		placedAt, _ := time.Parse(time.RFC3339, row.PlacedAt)
		pendings = append(pendings, reconcile.Pending{
			ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Side: domain.SideBuy,
			Trade: domain.TradeTypeCash, Quantity: row.Quantity, PlacedAt: placedAt,
		})
		byID[row.ClientOrderID] = row
	}
	resolutions := reconcile.Resolve(pendings, todays, reconcile.Options{Now: now, Grace: UnconfirmedGrace, Known: known})
	for _, r := range resolutions {
		row := byID[r.Pending.ClientOrderID]
		switch r.Outcome {
		case reconcile.Attributed:
			m := r.Match
			var amount *decimal.Decimal
			if m.AvgFillPrice != nil {
				a := row.Quantity.Mul(*m.AvgFillPrice)
				amount = &a
			}
			if err := led.UpdateStatusDetail(row.ClientOrderID, string(m.Status),
				&m.FilledQuantity, m.AvgFillPrice, m.BrokerOrderID, amount); err != nil {
				return err
			}
			reason := execution.ReasonExpired
			if m.FilledQuantity.IsPositive() {
				reason = execution.ReasonFilled
			}
			var fillPrice any
			if m.AvgFillPrice != nil {
				fillPrice = *m.AvgFillPrice
			}
			execution.Collect(execution.Spec{
				Event: "fill", App: "accum", Symbol: row.Symbol, Side: string(domain.SideBuy),
				ClientOrderID: row.ClientOrderID, BrokerOrderID: *m.BrokerOrderID, Live: true,
				Quantity: row.Quantity, FillQuantity: m.FilledQuantity, FillPrice: fillPrice, Reason: reason,
			})
			result.Changes = append(result.Changes, StatusChange{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Before: row.Status,
				After: m.Status, FilledQuantity: m.FilledQuantity, Quantity: row.Quantity,
			})
		case reconcile.NotSent:
			if err := led.UpdateStatus(row.ClientOrderID, string(domain.OrderStatusUnsent), nil, nil); err != nil {
				return err
			}
			execution.Collect(execution.Spec{
				Event: "fill", App: "accum", Symbol: row.Symbol, Side: string(domain.SideBuy),
				ClientOrderID: row.ClientOrderID, Live: true, Quantity: row.Quantity,
				Reason: execution.ReasonUnconfirmed,
			})
			result.Changes = append(result.Changes, StatusChange{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Before: row.Status,
				After: domain.OrderStatusUnsent, FilledQuantity: decimal.Zero, Quantity: row.Quantity,
			})
		case reconcile.Ambiguous:
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Status: row.Status, Reason: r.Reason,
			})
		case reconcile.TooRecent:
			// 次の run で判定する。保留として知らせるほどのことではない
		}
	}
	result.Resolved = reconcile.Summarize(resolutions)
	result.Resolutions = append(result.Resolutions, resolutions...)
	return nil
}
