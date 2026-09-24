package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/reconcile"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
)

// resolvePendingOrders は送信結果が分からず注文番号の無い PENDING を、当日の注文一覧と
// 突き合わせて判定し、台帳を直す（wbcore/reconcile）。
//
//   - 届いていた   → 注文番号と状態を書き戻す（未約定なら板に残っている注文として数える）
//   - 届いていない → UNSENT。WasPlaced が偽になり、差分があれば今回の実行で送り直す
//   - 決められない → PENDING のまま。発注は止める（同じ銘柄に二重に出さない）
//
// 一覧を照会できなければエラー（判定できないまま実弾を出さない）。
//
// daytrade（pkg/daytrade/execute.ResolvePending）と同じ形で塞ぐ:
//   - 今日送って注文番号まで分かっている注文が一覧に 1 つも無ければ、一覧が空で返った・
//     反映が遅れていると読み、「該当なし」を「届いていない」とはしない（reconcile.Options.Expected）。
//     一覧を信用して UNSENT にすると、届いていた注文を別の ID で送り直す（二重発注）
//   - 立花の一覧は当日分しか返らないので、今日より前に送った PENDING は判定しない
//     （一覧に無いのは当然で、UNSENT にすると送り直してしまう）。PENDING のまま残して警告する
func resolvePendingOrders(rep *repo.Repo, b broker.Broker, logger *logging.Logger, now time.Time) (reconcile.Summary, error) {
	var summary reconcile.Summary
	unresolved, err := rep.UnresolvedOrders()
	if err != nil {
		return summary, fmt.Errorf("未確定の注文を読めません: %w", err)
	}
	jst := clock.ToZone(now, clock.Tokyo)
	start := time.Date(jst.Year(), jst.Month(), jst.Day(), 0, 0, 0, 0, clock.Tokyo)
	end := start.AddDate(0, 0, 1)

	var pendings []reconcile.Pending
	var stale []string
	for _, o := range unresolved {
		if o.Status != domain.OrderStatusPending || o.BrokerOrderID != nil {
			continue
		}
		placedAt, perr := time.Parse(time.RFC3339, o.PlacedAt)
		if perr != nil || placedAt.Before(start) || !placedAt.Before(end) {
			stale = append(stale, o.ClientOrderID)
			continue
		}
		pendings = append(pendings, reconcile.Pending{
			ClientOrderID: o.ClientOrderID, Symbol: o.Symbol, Side: o.Side,
			Quantity: o.Quantity, PlacedAt: placedAt,
		})
	}
	if len(stale) > 0 {
		logger.Warn("wbjp.pending_unresolved", "今日より前に送った送信結果不明の注文は判定しない（一覧は当日分のみ。PENDING のまま）",
			map[string]any{"pending": len(stale), "client_order_ids": stale})
		digest.Anomaly("wbjp.pending_stale", fmt.Sprintf("今日より前の送信結果不明の注文 %d 件（口座の注文・約定と台帳を確かめてください）: %s",
			len(stale), strings.Join(stale, ", ")))
	}
	if len(pendings) == 0 {
		return summary, nil
	}
	todays, err := b.GetOrderHistory(start, end.Add(-time.Second))
	if err != nil {
		return summary, fmt.Errorf("送信結果不明の注文 %d 件を判定できません（当日の注文一覧を照会できない）: %w", len(pendings), err)
	}
	known, err := rep.BrokerOrderIDs()
	if err != nil {
		return summary, err
	}
	expected, err := rep.BrokerOrderIDsPlacedOn(jst.Format("2006-01-02"))
	if err != nil {
		return summary, fmt.Errorf("今日送った注文の番号を読めません: %w", err)
	}
	resolutions := reconcile.Resolve(pendings, todays, reconcile.Options{
		Now: now, Grace: reconcile.DefaultGrace, Known: known, Expected: expected})
	var ambiguous []string
	for _, r := range resolutions {
		fields := r.Fields()
		switch r.Outcome {
		case reconcile.Attributed:
			m := r.Match
			if err := rep.UpdateOrder(r.Pending.ClientOrderID, m.Status, m.FilledQuantity, m.AvgFillPrice, m.BrokerOrderID); err != nil {
				return summary, fmt.Errorf("判定の結果を台帳に書けません: %w", err)
			}
			logger.Info("wbjp.pending_resolved", "送信結果不明の注文は届いていた", fields)
		case reconcile.NotSent:
			if err := rep.UpdateOrder(r.Pending.ClientOrderID, domain.OrderStatusUnsent, decimalZero, nil, nil); err != nil {
				return summary, fmt.Errorf("判定の結果を台帳に書けません: %w", err)
			}
			logger.Info("wbjp.pending_resolved", "送信結果不明の注文は届いていなかった（送り直せる）", fields)
		case reconcile.Ambiguous:
			fields["fix"] = strings.Replace(fields["fix"].(string), "<app>", "wbjp", 1)
			logger.Error("wbjp.pending_ambiguous", "送信結果不明の注文を決められない（PENDING のまま）", fields)
			ambiguous = append(ambiguous, fmt.Sprintf("%s %s %s 株: %s", r.Pending.Symbol, r.Pending.Side, r.Pending.Quantity, r.Reason))
		case reconcile.TooRecent:
			logger.Info("wbjp.pending_resolved", "送った直後なので次の実行で判定する", fields)
		}
	}
	summary = reconcile.Summarize(resolutions)
	if len(ambiguous) > 0 {
		run.Alert("wbjp: 送信結果不明の注文を自動で決められません（口座の注文一覧を確かめてください）",
			strings.Join(ambiguous, "\n"))
	}
	return summary, nil
}
