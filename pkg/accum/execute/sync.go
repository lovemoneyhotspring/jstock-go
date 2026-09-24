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
// 埋まらない。ふつうは次の run がもう一度判定する。それでも決まらないものは
// ダイジェストの異常として残り、AI が口座の注文一覧と突き合わせる。
//
// NeedsResolve の行（前日以前に送った送信結果不明の注文）は次の run でも判定しない。
// `accum pending resolve` で人（か AI）が確定するまで残り、その銘柄は発注しない。
type UnresolvedOrder struct {
	ClientOrderID string
	Symbol        string
	Status        string
	Reason        string
	NeedsResolve  bool
}

func (u UnresolvedOrder) Describe() string {
	return fmt.Sprintf("%s（%s / %s）: %s", u.Symbol, u.ClientOrderID, u.Status, u.Reason)
}

// SyncResult は照会の結果。
type SyncResult struct {
	// Changes は台帳を更新できた注文。
	Changes []StatusChange
	// Unresolved は照会できず保留した注文。空でなければ知らせる（ふつうは次の run で再判定。
	// NeedsResolve の行は `accum pending resolve` まで残る）。
	Unresolved []UnresolvedOrder
	// Resolved は送信結果不明（PENDING）の注文を当日の注文一覧で判定した集計。
	Resolved reconcile.Summary
	// Resolutions はその 1 件ごとの判定（ログに項目付きで残す）。
	Resolutions []reconcile.Resolution
}

// SyncOrderStatus は結果が確定していない注文をブローカーに照会し、台帳を更新する。
//
// ブローカーに無い注文は原則そのまま残す（勝手に「失効」にすると、実は板に
// 残っていた注文と二重になる）。例外は**今日送った送信中（PENDING）のまま
// UnconfirmedGrace を過ぎても当日の注文一覧に無い**注文——応答が返らず記録だけが
// 残ったもの。一覧を信用できるときに限り UNSENT に落とし、次の実行で差額として
// 埋め直す（条件は resolveUnconfirmed）。前日以前の PENDING は一覧に出ないので落とさない。
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
			// ブローカーが知らない。勝手に失効へ倒さず保留にする。
			// 受理済み（SUBMITTED 等）でも注文番号が無い行はありうる（受理の応答に番号が無かった）ので、
			// 番号を前提に参照しない——nil を辿って run ごと落ちる
			reason := "ブローカーの応答に無い（注文番号なし）"
			if row.BrokerOrderID != nil {
				reason = "注文番号 " + *row.BrokerOrderID + " がブローカーの応答に無い"
			}
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol,
				Status: row.Status, Reason: reason,
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
//
// **「一覧に無い」を「届いていない」と読むのは、一覧を信用できるときだけ**（2026-09-24 のレビュー A1）:
//
//   - 前日以前に送った PENDING は判定しない（保留）。立花の注文一覧（CLMOrderList）は
//     当日分だけを返すものとして扱っている（tachibana_orders.go の GetOrderHistory。
//     前日以前を返すかは実機で確かめていない）。前日の当日限りの注文は、約定していても
//     今日の一覧に出ないかもしれない。「無い」を UNSENT にすると約定済みの注文をもう一度
//     買う。一覧の中身によらず決めない。送った日が読めない行も同じ扱い
//   - 今日送って注文番号まで分かっている注文（Expected）がどれも一覧に無ければ、一覧が
//     空で返った・反映が遅れていると読み、判定を先送りする（reconcile.Options.Expected。
//     daytrade と同じ塞ぎ方）
//   - 一覧が 0 件なら判定しない（保留）。積立は 1 日の注文が数件で Expected が空の日が
//     ほとんどなので、上の目印だけでは「空で返った一覧」を見分けられない
//
// 保留した PENDING は台帳に残り、run はその銘柄を発注しない（RunAccumulation）。人（か AI）が
// 口座の約定履歴で確かめ、`accum pending resolve` で確定する。
func resolveUnconfirmed(led *ledger.Ledger, b broker.Broker, rows []ledger.LedgerOrder, now time.Time, result *SyncResult) error {
	hold := func(rows []ledger.LedgerOrder, reason string, needsResolve bool) {
		for _, row := range rows {
			result.Unresolved = append(result.Unresolved, UnresolvedOrder{
				ClientOrderID: row.ClientOrderID, Symbol: row.Symbol, Status: row.Status, Reason: reason,
				NeedsResolve: needsResolve,
			})
		}
	}
	jst := clock.ToZone(now, clock.Tokyo)
	start := time.Date(jst.Year(), jst.Month(), jst.Day(), 0, 0, 0, 0, clock.Tokyo)
	end := start.AddDate(0, 0, 1)

	var todaysRows, earlier []ledger.LedgerOrder
	for _, row := range rows {
		placedAt, err := time.Parse(time.RFC3339, row.PlacedAt)
		if err != nil || placedAt.Before(start) || !placedAt.Before(end) {
			earlier = append(earlier, row)
			continue
		}
		todaysRows = append(todaysRows, row)
	}
	if len(earlier) > 0 {
		hold(earlier, "今日より前に送った送信結果不明の注文。立花の注文一覧は前日以前を返さないことがあるので、"+
			"一覧に無くても「届いていない」とは決めない（口座の約定履歴で確かめて `accum pending resolve`）", true)
	}
	if len(todaysRows) == 0 {
		return nil
	}
	rows = todaysRows

	todays, err := b.GetOrderHistory(start, jst)
	if err != nil {
		hold(rows, "当日の注文一覧を照会できない: "+err.Error(), false)
		return nil
	}
	if len(todays) == 0 {
		// 送信から猶予を過ぎていないものは一覧に載る前なので、いつもどおり次の run で判定する
		var listEmpty []ledger.LedgerOrder
		for _, row := range rows {
			placedAt, _ := time.Parse(time.RFC3339, row.PlacedAt)
			if now.Sub(placedAt) >= UnconfirmedGrace {
				listEmpty = append(listEmpty, row)
			}
		}
		hold(listEmpty, "当日の注文一覧が 0 件（届いていないのか一覧が空で返ったのか区別できない）。"+
			"口座の注文照会で確かめて `accum pending resolve`", false)
		return nil
	}
	known, err := led.BrokerOrderIDs()
	if err != nil {
		return err
	}
	expected, err := led.BrokerOrderIDsPlacedBetween(start, end)
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
	resolutions := reconcile.Resolve(pendings, todays, reconcile.Options{
		Now: now, Grace: UnconfirmedGrace, Known: known, Expected: expected})
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
