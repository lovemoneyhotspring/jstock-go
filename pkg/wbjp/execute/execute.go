// Package execute は wbjp の発注の手順。
//
//   - 送る前に台帳へ PENDING を書き、結果で書き換える（PlaceRecorded）
//   - 目標との差分の注文を審査して順に出し、受理したぶんを余力・未約定に反映する（PlaceOrders）
//   - 出した注文の約定をブローカーに照会して台帳へ取り込む（SyncFills）
//   - 取消を送り、台帳に残す（CancelRecorded）
//
// daytrade の execute.PlaceRecorded と同じ規則で書いてある（二重発注より出し漏れの方がまし）。
package execute

import (
	"errors"
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// DryRunStatus は dry-run で「出したつもり」の注文を台帳に残すときの状態。
const DryRunStatus = "dry_run"

// Reporter はログの出し先。*logging.Logger が満たす。
type Reporter interface {
	Info(code, msg string, extra ...map[string]any)
	Warn(code, msg string, extra ...map[string]any)
	Error(code, msg string, extra ...map[string]any)
}

// ErrUnconfirmedOrder は送ったが結果が分からない注文。届いている可能性があるので、
// 台帳は送信中（PENDING）のまま残し、次の実行の判定に回す。これが出たら発注を止める。
type ErrUnconfirmedOrder struct {
	ClientOrderID string
	Err           error
}

func (e *ErrUnconfirmedOrder) Error() string {
	return fmt.Sprintf("注文 %s の結果を確認できませんでした（送信済みの可能性）: %v", e.ClientOrderID, e.Err)
}

func (e *ErrUnconfirmedOrder) Unwrap() error { return e.Err }

// PlaceRecorded は送る前に台帳へ PENDING を書き、送ったら結果で書き換える。
//
//   - 受理された               → その状態と注文番号で上書き。書けなければ ErrUnconfirmedOrder
//   - 締め切り・送る前の失敗   → UNSENT（届いた可能性が無いので次回送り直せる）。元のエラーを返す
//   - 明確に拒否された         → REJECTED（PENDING のままだと次回 WasPlaced で弾かれ二度と出ない）
//   - それ以外（通信断など）   → PENDING のまま ErrUnconfirmedOrder
func PlaceRecorded(rep *repo.Repo, b broker.Broker, runID string, req domain.OrderRequest, report Reporter) error {
	if err := rep.RecordOrder(runID, req, string(domain.OrderStatusPending), nil); err != nil {
		return fmt.Errorf("発注前の記録に失敗しました（発注を中止します）: %w", err)
	}
	ack, err := b.Place(req)
	if err != nil {
		var deadline *broker.ErrDeadline
		var notSent *broker.ErrNotSent
		if errors.As(err, &deadline) || errors.As(err, &notSent) {
			if uerr := rep.UpdateOrder(req.ClientOrderID, domain.OrderStatusUnsent, decimal.Zero, nil, nil); uerr != nil {
				report.Error("wbjp.ledger", fmt.Sprintf("%s: 未送信を記録できません（台帳は送信中のまま）: %v", req.Symbol, uerr))
			}
			return err
		}
		var rejected *broker.OrderRejectedError
		if errors.As(err, &rejected) {
			if uerr := rep.UpdateOrder(req.ClientOrderID, domain.OrderStatusRejected, decimal.Zero, nil, nil); uerr != nil {
				report.Error("wbjp.ledger", fmt.Sprintf("%s: 拒否を記録できません（台帳は送信中のまま）: %v", req.Symbol, uerr))
			}
			return err
		}
		return &ErrUnconfirmedOrder{ClientOrderID: req.ClientOrderID, Err: err}
	}
	if uerr := rep.UpdateOrder(req.ClientOrderID, ack.Status, decimal.Zero, nil, ack.BrokerOrderID); uerr != nil {
		// 注文は出ている。注文番号が台帳に残らないと照会も取消もできない
		return &ErrUnconfirmedOrder{ClientOrderID: req.ClientOrderID, Err: fmt.Errorf(
			"発注は受理されました（注文番号 %s）が台帳の更新に失敗しました: %w", stringOf(ack.BrokerOrderID), uerr)}
	}
	return nil
}

// sentNothing は「送っていない・受け付けられていない」ことが確かなエラーか。
// これらは台帳が UNSENT / REJECTED になっているので、残りの注文へ進んでよい。
func sentNothing(err error) bool {
	var deadline *broker.ErrDeadline
	var notSent *broker.ErrNotSent
	var rejected *broker.OrderRejectedError
	return errors.As(err, &deadline) || errors.As(err, &notSent) || errors.As(err, &rejected)
}

// Options は PlaceOrders の指定。
type Options struct {
	RunID string
	// Live が偽なら送らず、台帳に dry_run として残すだけ。
	Live   bool
	Risk   *risk.RiskManager
	Report Reporter
}

// Result は PlaceOrders の結果。
type Result struct {
	// Placed は受理された注文の数（dry-run では記録した数）。
	Placed int
	// AlreadyPlaced は発注済みで飛ばした注文の数。
	AlreadyPlaced int
	// RiskRejected はリスク審査で見送った銘柄 → 理由。台帳の risk_events に残す。
	RiskRejected map[string]string
	// Failed はブローカーが受け付けなかった（拒否・未送信）注文の説明。
	Failed []string
}

// PlaceOrders は注文を順に審査して出す。
//
// 受理した注文ごとに riskCtx を進める——当日の件数だけでなく、買いなら買付余力を
// 減らし未約定の買いに足す。進めないと、余力 100 万円に 40 万円の買いを 3 本
// 通してしまう（1 本ずつ見れば全部余力の内側）。dry-run も同じだけ進め、
// 実際に出したときと同じ判断を記録する。
//
// 止まる（エラーを返す）のは、台帳が読めない・書けないときと、送った結果が
// 分からないとき。拒否・未送信は記録して次の注文へ進む。
func PlaceOrders(rep *repo.Repo, b broker.Broker, orders []domain.OrderRequest, riskCtx *risk.RiskContext, opts Options) (Result, error) {
	result := Result{RiskRejected: map[string]string{}}
	for _, req := range orders {
		// 送信後・記録前に落ちた注文を再送しない。台帳が読めなければ判断できないので止める
		placed, err := rep.WasPlaced(req.ClientOrderID)
		if err != nil {
			return result, fmt.Errorf("台帳を読めないため発注を中止しました（二重発注を避けます）: %w", err)
		}
		if placed {
			result.AlreadyPlaced++
			opts.Report.Info("wbjp.skip", fmt.Sprintf("%s: 既に発注済み (ID: %s)", req.Symbol, req.ClientOrderID))
			continue
		}

		decision := opts.Risk.Check(req, *riskCtx, nil)
		if !decision.Approved {
			if prev, ok := result.RiskRejected[req.Symbol]; ok {
				result.RiskRejected[req.Symbol] = prev + "; " + decision.Reason
			} else {
				result.RiskRejected[req.Symbol] = decision.Reason
			}
			opts.Report.Warn("wbjp.risk_rejected", fmt.Sprintf("%s 発注見送り: %s", req.Symbol, decision.Reason))
			continue
		}

		if !opts.Live {
			if err := rep.RecordOrder(opts.RunID, req, DryRunStatus, nil); err != nil {
				opts.Report.Warn("wbjp.ledger", fmt.Sprintf("%s: dry-run を記録できません: %v", req.Symbol, err))
			}
			opts.Report.Info("wbjp.dry_run", fmt.Sprintf("[dry-run] %s %s %s株 @ %s円 (%s)",
				req.Symbol, req.Side, req.Quantity, priceText(req.LimitPrice), req.Reason))
			Reserve(riskCtx, req)
			result.Placed++
			continue
		}

		if err := PlaceRecorded(rep, b, opts.RunID, req, opts.Report); err != nil {
			if sentNothing(err) {
				opts.Report.Error("wbjp.order_failed", fmt.Sprintf("%s 発注できません: %v", req.Symbol, err))
				result.Failed = append(result.Failed, fmt.Sprintf("%s %s %s株: %v", req.Symbol, req.Side, req.Quantity, err))
				continue
			}
			opts.Report.Error("wbjp.unconfirmed", fmt.Sprintf("%s: %v", req.Symbol, err))
			return result, err
		}
		opts.Report.Info("wbjp.order", fmt.Sprintf("発注成功: %s %s %s株 (ID: %s)", req.Symbol, req.Side, req.Quantity, req.ClientOrderID))
		Reserve(riskCtx, req)
		result.Placed++
	}
	return result, nil
}

// Reserve は通した注文のぶんだけリスクの前提を進める。
//
// 件数は売買とも 1 件。買いは概算の約定代金を買付余力から引き、未約定の買いに足す
// （売りの代金は約定・受渡まで余力に戻らないので足さない）。
func Reserve(ctx *risk.RiskContext, req domain.OrderRequest) {
	ctx.OrdersToday++
	if req.Side != domain.SideBuy {
		return
	}
	notional := risk.Notional(req, ctx.BasePrices)
	ctx.Balance.BuyingPower = ctx.Balance.BuyingPower.Sub(notional)
	if ctx.PendingValue == nil {
		ctx.PendingValue = map[string]decimal.Decimal{}
	}
	ctx.PendingValue[req.Symbol] = ctx.PendingValue[req.Symbol].Add(notional)
}

// FillChange は照会で分かった注文の変化。
type FillChange struct {
	ClientOrderID  string
	Symbol         string
	Before         domain.OrderStatus
	After          domain.OrderStatus
	FilledQuantity decimal.Decimal
	Quantity       decimal.Decimal
}

// FillSync は SyncFills の結果。
type FillSync struct {
	Changes []FillChange
	// Unresolved は照会できず保留した注文の説明。台帳はそのまま（次の実行で再照会）。
	Unresolved []string
}

// SyncFills は注文番号のある未確定の注文をブローカーに照会し、約定・失効を台帳に取り込む。
//
// 取り込まないと SUBMITTED のまま残り続け、
//   - 約定数量が 0 のままなので当日買付（BoughtToday）が空になり、差金決済の柵が効かない
//   - 未約定の買い（PendingBuyValue）に積み上がり、いずれ比率上限で全ての買いが止まる
//
// 注文番号の無い PENDING は client_order_id で引けない（立花証券）ので対象外
// （resolvePendingOrders が当日の注文一覧で判定する）。1 件照会できないだけで残りを
// 止めず保留にする。ブローカーが知らない注文を勝手に失効へ倒さない（板に残っていたら二重になる）。
func SyncFills(rep *repo.Repo, b broker.Broker) (FillSync, error) {
	var result FillSync
	orders, err := rep.UnresolvedOrders()
	if err != nil {
		return result, fmt.Errorf("未確定の注文を読めません: %w", err)
	}
	for _, o := range orders {
		if o.BrokerOrderID == nil {
			continue
		}
		got, err := b.GetOrder(o.ClientOrderID, o.BrokerOrderID)
		if err != nil {
			result.Unresolved = append(result.Unresolved, fmt.Sprintf("%s（%s）: %v", o.Symbol, o.ClientOrderID, err))
			continue
		}
		if got == nil {
			result.Unresolved = append(result.Unresolved,
				fmt.Sprintf("%s（%s）: 注文番号 %s がブローカーの応答に無い", o.Symbol, o.ClientOrderID, *o.BrokerOrderID))
			continue
		}
		if got.Status == "" || got.Status == domain.OrderStatusUnknown {
			result.Unresolved = append(result.Unresolved,
				fmt.Sprintf("%s（%s）: 状態が分からない（%q）", o.Symbol, o.ClientOrderID, got.Status))
			continue
		}
		// 約定数量は減らない。照会の応答が欠けていても記録済みの約定を消さない
		filled := decimal.Max(got.FilledQuantity, o.FilledQuantity)
		if got.Status == o.Status && filled.Equal(o.FilledQuantity) {
			continue
		}
		brokerID := got.BrokerOrderID
		if brokerID == nil {
			brokerID = o.BrokerOrderID
		}
		if err := rep.UpdateOrder(o.ClientOrderID, got.Status, filled, got.AvgFillPrice, brokerID); err != nil {
			return result, fmt.Errorf("注文 %s の約定を台帳に書けません: %w", o.ClientOrderID, err)
		}
		result.Changes = append(result.Changes, FillChange{
			ClientOrderID: o.ClientOrderID, Symbol: o.Symbol,
			Before: o.Status, After: got.Status,
			FilledQuantity: filled, Quantity: o.Quantity,
		})
	}
	return result, nil
}

// CancelResult は CancelRecorded の結果。
type CancelResult struct {
	// Recorded は台帳に記録のある注文だったか。偽なら取消は送ったが台帳には何も書いていない。
	Recorded bool
	// Status は台帳に書いた（または書かずに残した）状態。
	Status domain.OrderStatus
	// Deferred は取消がまだ反映されておらず、台帳を書き換えずに残したか（次の約定同期で反映）。
	Deferred bool
	// Unverified は取消の後に注文を照会できず、取り消せたかを確かめられなかったか。
	// Deferred と同じく台帳は書き換えない。QueryErr はそのときの照会のエラー。
	Unverified bool
	QueryErr   error
}

// CancelRecorded は取消を送り、台帳に残す。
//
// 取消を送ったら注文を照会し、終了状態ならその状態と約定数量を書く（部分約定の
// 取消で約定分を 0 に戻さない）。取消がまだ反映されていない（板に残っている）か、
// 照会できず確かめられないなら台帳は書き換えず、次の run の約定同期に任せる——
// 先に CANCELLED と書くと、取消が間に合わず約定していた場合にその約定が二度と
// 取り込まれない（CANCELLED は終了状態なので約定同期の対象から外れる）。
func CancelRecorded(rep *repo.Repo, b broker.Broker, clientOrderID string) (CancelResult, error) {
	var result CancelResult
	rec, err := rep.GetOrder(clientOrderID)
	if err != nil {
		return result, fmt.Errorf("台帳の注文 %s を読めません: %w", clientOrderID, err)
	}
	var brokerID *string
	if rec != nil {
		if string(rec.Status) == DryRunStatus {
			return result, fmt.Errorf("注文 %s は dry-run の記録で、発注していません", clientOrderID)
		}
		if rec.Status.IsTerminal() {
			return result, fmt.Errorf("注文 %s は既に終わっています（%s）", clientOrderID, rec.Status)
		}
		brokerID = rec.BrokerOrderID
	}

	if err := b.Cancel(clientOrderID, brokerID); err != nil {
		return result, fmt.Errorf("取消に失敗しました: %w", err)
	}
	if rec == nil {
		return result, nil
	}
	result.Recorded = true

	got, qerr := b.GetOrder(clientOrderID, brokerID)
	switch {
	case qerr != nil || got == nil:
		// 取り消せたかを確かめられない。状態を確定させない（約定同期が照会し直す）
		if qerr == nil {
			qerr = fmt.Errorf("注文 %s の照会結果が空です", clientOrderID)
		}
		result.Status = rec.Status
		result.Deferred = true
		result.Unverified = true
		result.QueryErr = qerr
	case got.Status.IsTerminal():
		filled := decimal.Max(got.FilledQuantity, rec.FilledQuantity)
		if err := rep.UpdateOrder(clientOrderID, got.Status, filled, got.AvgFillPrice, got.BrokerOrderID); err != nil {
			return result, fmt.Errorf("取消は送りましたが台帳に書けません: %w", err)
		}
		result.Status = got.Status
	default:
		result.Status = rec.Status
		result.Deferred = true
	}
	return result, nil
}

func priceText(p *decimal.Decimal) string {
	if p == nil {
		return "成行"
	}
	return p.String()
}

func stringOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
