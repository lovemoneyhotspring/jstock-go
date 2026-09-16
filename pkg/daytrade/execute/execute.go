// Package execute はデイトレの発注——寄付の建玉、引けの手仕舞い、照会の突き合わせ。
//
// cmd/daytrade から切り出してあるのは、発注の規則（台帳に PENDING を書いてから送る、
// 拒否は REJECTED、結果不明は PENDING のまま、台帳外の建玉があれば止める）を
// 模型のブローカーと一時的な台帳で検証するため。画面への出力は Env.Out、
// ログと通知は Env.Report を通す。ダイジェスト（digest）への記録は呼び出し側の役目。
package execute

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/reconcile"
	"github.com/shopspring/decimal"
)

// Reporter はログと通知の出口。*cli.Run がこれを満たす。
type Reporter interface {
	Info(code, msg string, extra ...map[string]any)
	Warn(code, msg string, extra ...map[string]any)
	Error(code, msg string, extra ...map[string]any)
	Alert(title, body string)
}

// Env は 1 回の実行で共有する道具。
type Env struct {
	Cfg    config.Config
	Ledger *ledger.Ledger
	Day    time.Time
	Report Reporter
	// Out は人向けの出力。nil なら捨てる。
	Out io.Writer
	// RetryWait は送信結果が分からなかったとき、当日の注文一覧で判定するまでに待つ時間
	// （受付が一覧に載るまでの猶予）。テストは 0。
	RetryWait time.Duration
	// Deadline はこの実行の締め切り（config.Execution.RunDeadline）。過ぎたら新しい注文は
	// 送らず「締め切り」として見送る。ゼロ値なら締め切りなし。ブローカー側の締め切り
	// （broker.SetDeadline）と同じ値を渡す——こちらは「次を始めない」、あちらは
	// 「送信中を打ち切る」の役割。
	Deadline time.Time
	// RefPrices は発注ループの前にまとめて取った「送る直前の時価」（銘柄 → 時価）。
	// 1 銘柄ずつ聞くと注文の合間に往復が直列で挟まり、測ろうとしている遅れ自体を増やす
	// ——時価問合は 1 回 120 銘柄まで束ねられる（2026-09-16 のレビュー）。
	// nil なら 1 銘柄ずつ聞き直す（持ち越しの返済など、pick を持たない経路）。
	RefPrices map[string]broker.MarketPrice
}

// DefaultRetryWait は RetryWait の既定。
const DefaultRetryWait = 5 * time.Second

func (e Env) printf(format string, a ...any) {
	if e.Out != nil {
		fmt.Fprintf(e.Out, format, a...)
	}
}

// expired は締め切りを過ぎているか。
func (e Env) expired() bool {
	return !e.Deadline.IsZero() && !clock.NowUTC().Before(e.Deadline)
}

// deadlineText は人向けの締め切り（JST）。
func (e Env) deadlineText() string {
	return clock.ToZone(e.Deadline, clock.Tokyo).Format("15:04:05")
}

// boundedWait は d 待つ。締め切りが先に来るならそこまでしか待たない。
// d を丸ごと待てたか（締め切りで縮めなかったか）を返す。
func (e Env) boundedWait(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	full := true
	if !e.Deadline.IsZero() {
		if remaining := e.Deadline.Sub(clock.NowUTC()); remaining < d {
			d, full = remaining, false
		}
	}
	if d > 0 {
		time.Sleep(d)
	}
	return full
}

// Placed は今日すでに建てた（生きている／約定した）建玉の数。拒否・失効・dry-run は数えない。
type Placed struct {
	Long  int
	Short int
	// LongAmount / ShortAmount は建てた金額（株数 × 判断時の値段。値段が無ければ約定単価）。
	// 再実行が「残りの資金」を件数ではなく金額で数えるために使う（SizeDay）——候補が N に
	// 満たない日は 1 銘柄に 1 注文の予算を超えて入るので、件数だけでは使った資金が分からない。
	LongAmount  decimal.Decimal
	ShortAmount decimal.Decimal
	// Symbols は建てた銘柄 → 向き。同じ銘柄を同じ日に重ねて建てないために使う。
	Symbols map[string]domain.Side
}

// Total は長短の合計。
func (p Placed) Total() int { return p.Long + p.Short }

// PlacedToday は台帳から今日の建玉の数を数える。
//
// 再実行は「残りの枚数だけ建て直す」——1 回目が途中で落ちても（通信エラー・締め切り）、
// 次の cron が N − 発注済みぶんを埋める。1 回目に建てた銘柄は候補から外す。
func PlacedToday(env Env) (Placed, error) {
	entries, err := env.Ledger.EntriesOn(env.Day)
	if err != nil {
		return Placed{}, err
	}
	placed := Placed{Symbols: map[string]domain.Side{}}
	for _, o := range entries {
		if o.IsDryRun() || o.IsDead() {
			continue
		}
		amount := decimal.Zero
		if o.Price != nil {
			amount = o.Quantity.Mul(*o.Price)
		} else if o.AvgFillPrice != nil {
			amount = o.Quantity.Mul(*o.AvgFillPrice)
		}
		if o.Side == domain.SideBuy {
			placed.Long++
			placed.LongAmount = placed.LongAmount.Add(amount)
		} else {
			placed.Short++
			placed.ShortAmount = placed.ShortAmount.Add(amount)
		}
		placed.Symbols[o.Symbol] = o.Side
	}
	return placed, nil
}

func (e Env) dayText() string { return e.Day.Format(cli.DateLayout) }

// ErrUnconfirmedOrder は送信したが結果を確認できなかった注文。
//
// 通信断やタイムアウトでは「届いていない」と「届いたが応答が返らない」を区別できない。
// 台帳には送信中（PENDING）が残るので、次の実行は WasPlaced で弾かれて再送されない。
// 発注は受理されたが台帳を更新できなかった場合も同じ扱い——注文番号が残らないと
// 照会も取消もできないので、人が確かめるまで止める。
type ErrUnconfirmedOrder struct {
	ClientOrderID string
	Err           error
}

func (e *ErrUnconfirmedOrder) Error() string {
	return fmt.Sprintf("注文 %s の結果を確認できませんでした（送信済みの可能性があります）: %v",
		e.ClientOrderID, e.Err)
}

func (e *ErrUnconfirmedOrder) Unwrap() error { return e.Err }

// ErrUnrecordedPositions は台帳に無い建玉がブローカーにあった。発注は中止している。
type ErrUnrecordedPositions struct {
	Positions  []string
	LedgerPath string
}

func (e *ErrUnrecordedPositions) Error() string {
	return fmt.Sprintf("台帳に無い建玉があります（二重に建てます）: %s\n"+
		"台帳（%s）が失われているか、別の環境で発注した可能性があります。"+
		"口座を確かめてから実行してください", strings.Join(e.Positions, "、"), e.LedgerPath)
}

// EntryRequest は建てる注文。ロング（BUY）は現物か信用買い、ショート（SELL）は信用新規売り。
func EntryRequest(pick selection.Pick, day time.Time, cfg config.Config, attempt int) domain.OrderRequest {
	// 前回が拒否されていたら種を変える（同じ ID はブローカーが弾く）。attempt 0 は従来と同じ ID
	seed := "daytrade|" + day.Format(cli.DateLayout)
	if attempt > 0 {
		seed = fmt.Sprintf("%s|%d", seed, attempt)
	}
	trade := EntryTrade(pick.Side, cfg)
	action := "買い"
	if pick.Side == domain.SideSell {
		action = "売建"
	}
	gap, _ := pick.Gap.Float64()
	return domain.OrderRequest{
		ClientOrderID: domain.MakeClientOrderID(seed, pick.Symbol, pick.Side, pick.Quantity),
		Symbol:        pick.Symbol,
		Side:          pick.Side,
		OrderType:     domain.OrderTypeMarket,
		Quantity:      pick.Quantity,
		TaxType:       cfg.Execution.TaxAccountType,
		Reason: fmt.Sprintf("%s %s gap %s #%d %s",
			cfg.StrategyName(), day.Format(cli.DateLayout), cli.Pct(gap), pick.Rank, action),
		Trade: trade,
	}
}

// EntryTrade はその脚を建てるときの売買区分。
//
//   - 売り     … 信用新規売り（売建。現物では空売りできない）
//   - 買い     … long_via_margin なら信用買い（日計りは手数料 0 円）、そうでなければ現物
//
// 建玉をブローカーに照会して突き合わせるとき、現物と信用を取り違えないために使う。
func EntryTrade(side domain.Side, cfg config.Config) domain.TradeType {
	if side == domain.SideSell || (cfg.Margin.Enabled && cfg.Margin.LongViaMargin) {
		return domain.TradeTypeMarginOpen
	}
	return domain.TradeTypeCash
}

// refPriceOf は**送る直前**の時価。約定単価と比べると執行そのものの滑りが出る。
//
// 選定に使った 9:00 の気配では、判断から発注までの遅れ（危険信号の再試行や余力の照会で
// 数分空く）が混じって滑りと区別できない。だから送る直前に 1 銘柄ずつ聞き直す。
// 取れなければ nil——記録のためだけの値なので、発注は止めない。
// prefetchRefPrices は発注の前に pick の時価をまとめて取る（1 リクエスト 120 銘柄まで）。
// 取れなければ nil を返し、各注文が従来どおり 1 銘柄ずつ聞き直す——記録のための値なので、
// ここで発注を止めることはしない。
func prefetchRefPrices(env Env, b broker.Broker, picks []selection.Pick) map[string]broker.MarketPrice {
	source, ok := b.(broker.PriceSource)
	if !ok || len(picks) == 0 {
		return nil
	}
	symbols := make([]string, 0, len(picks))
	seen := make(map[string]bool, len(picks))
	for _, p := range picks {
		if seen[p.Symbol] {
			continue
		}
		seen[p.Symbol] = true
		symbols = append(symbols, p.Symbol)
	}
	prices, err := source.MarketPrices(symbols)
	if err != nil {
		env.Report.Warn("daytrade.ref_price", "執行時の時価をまとめて取れません（1 銘柄ずつ聞き直します）", map[string]any{
			"day": env.dayText(), "symbols": len(symbols), "error": err.Error(),
		})
		return nil
	}
	return prices
}

func refPriceOf(env Env, b broker.Broker, symbol string) *ledger.Ref {
	price, ok := env.RefPrices[symbol]
	if !ok {
		source, isSource := b.(broker.PriceSource)
		if !isSource {
			return nil
		}
		prices, err := source.MarketPrices([]string{symbol})
		if err != nil {
			env.Report.Warn("daytrade.ref_price", "執行時の時価を取れません（発注は続けます）", map[string]any{
				"day": env.dayText(), "symbol": symbol, "error": err.Error(),
			})
			return nil
		}
		// 応答に銘柄が無いのは値が無いのと違う。黙ると、銘柄コードの形式が食い違って
		// 全件外れたときに ref が永久に空のまま誰も気づかない
		if price, ok = prices[symbol]; !ok {
			env.Report.Info("daytrade.ref_price", "執行時の時価に銘柄が無い（発注は続けます）", map[string]any{
				"day": env.dayText(), "symbol": symbol,
			})
			return nil
		}
	}
	ref := ledger.Ref{Price: price.Last, Bid: price.Bid, Ask: price.Ask, At: price.At}
	if !ref.Price.IsPositive() {
		// 寄り前と未寄付の銘柄は現在値が無い。値段は気配にしかない（docs/OPENING_DATA.md）
		ref.Price = midOf(price.Bid, price.Ask)
	}
	if !ref.Price.IsPositive() {
		return nil
	}
	return &ref
}

// midOf は最良気配の仲値。片側しか無ければその側の値。
func midOf(bid, ask decimal.Decimal) decimal.Decimal {
	switch {
	case bid.IsPositive() && ask.IsPositive():
		return bid.Add(ask).Div(decimal.NewFromInt(2))
	case bid.IsPositive():
		return bid
	default:
		return ask
	}
}

// PlaceRecorded は送る前に台帳へ PENDING を書き、送ったら結果で更新する。
//
// 送信後に落ちても台帳には残るので、次の実行で同じ注文を送り直さない
// （二重買付より買い漏れの方がまし）。
//
//   - 受理された           → その状態で上書き。上書きできなければ ErrUnconfirmedOrder
//   - 明確に拒否された     → REJECTED。PENDING のままだと「送信結果不明」と区別できない
//   - それ以外（通信断等） → 送信中のまま残し ErrUnconfirmedOrder
//
// 同時に実行品質の intent 行を残す。台帳の price は後で約定額に上書きされうるので、
// 判断時の想定はここで別に控えておく。
func PlaceRecorded(env Env, b broker.Broker, request domain.OrderRequest, price decimal.Decimal, fee *decimal.Decimal) error {
	// 送る直前の時価。intent 行と台帳の両方に残す（closure は後で読むので先に宣言する）
	var ref *ledger.Ref
	intent := func(reason execution.ReasonCode, note string) {
		refPrice := any(nil)
		if ref != nil {
			refPrice = ref.Price
		}
		execution.Collect(execution.Spec{
			Event: execution.EventIntent, App: "daytrade",
			Symbol: request.Symbol, Side: string(request.Side), Trade: string(request.Trade),
			ClientOrderID: request.ClientOrderID, Live: true,
			Quantity:     request.Quantity,
			IntentPrice:  price,
			IntentAmount: price.Mul(request.Quantity),
			IntentFee:    fee,
			RefPrice:     refPrice,
			Reason:       reason, Note: note,
		})
	}
	if err := env.Ledger.Record(request, env.Day, string(domain.OrderStatusPending), &price, nil); err != nil {
		return fmt.Errorf("発注前の台帳記録に失敗しました（発注を中止します）: %w", err)
	}
	if ref = refPriceOf(env, b, request.Symbol); ref != nil {
		if err := env.Ledger.SetRef(request.ClientOrderID, *ref); err != nil {
			// 測るための値なので、記録できなくても発注は続ける
			env.Report.Warn("daytrade.ref_price", "執行時の時価を台帳に残せません（発注は続けます）", map[string]any{
				"day": env.dayText(), "symbol": request.Symbol, "error": err.Error(),
			})
		}
	}
	ack, err := b.Place(request)
	if err != nil {
		var deadline *broker.ErrDeadline
		var notSent *broker.ErrNotSent
		if errors.As(err, &deadline) || errors.As(err, &notSent) {
			// 締め切り、または送る前の失敗（返済する建玉の照会が落ちた等）で**送っていない**。
			// 届いた可能性は無いので UNSENT にして次の判断に譲る
			// （PENDING のままだと一覧照会で判定するまで再送できず、引けの締め切りに間に合わない）
			if uerr := env.Ledger.UpdateStatus(request.ClientOrderID, domain.OrderStatusUnsent, decimal.Zero, nil, nil); uerr != nil {
				env.Report.Error("daytrade.ledger", "未送信を記録できません（台帳は送信中のまま）", map[string]any{
					"client_order_id": request.ClientOrderID, "symbol": request.Symbol, "error": uerr.Error(),
				})
			}
			reason := execution.ReasonWindowClosed
			if notSent != nil {
				reason = execution.ReasonBrokerError
			}
			intent(reason, err.Error())
			return err
		}
		var rejected *broker.OrderRejectedError
		if errors.As(err, &rejected) {
			if uerr := env.Ledger.UpdateStatus(request.ClientOrderID, domain.OrderStatusRejected, decimal.Zero, nil, nil); uerr != nil {
				// PENDING のまま残ると次回 WasPlaced で弾かれ、拒否された建玉が送り直されない
				env.Report.Error("daytrade.ledger", "拒否を記録できません（台帳は送信中のまま）", map[string]any{
					"client_order_id": request.ClientOrderID, "symbol": request.Symbol, "error": uerr.Error(),
				})
			}
			intent(execution.ReasonBrokerError, err.Error())
			return err
		}
		intent(execution.ReasonUnconfirmed, err.Error())
		return &ErrUnconfirmedOrder{ClientOrderID: request.ClientOrderID, Err: err}
	}
	if uerr := env.Ledger.UpdateStatus(request.ClientOrderID, ack.Status, decimal.Zero, nil, ack.BrokerOrderID); uerr != nil {
		// 注文は出ている。注文番号が台帳に残らないと close / verify / 取消ができない
		env.Report.Error("daytrade.ledger", "発注は受理されたが台帳を更新できません", map[string]any{
			"client_order_id": request.ClientOrderID, "symbol": request.Symbol,
			"broker_order_id": stringOf(ack.BrokerOrderID), "error": uerr.Error(),
		})
		intent(execution.ReasonUnconfirmed, "台帳の更新に失敗: "+uerr.Error())
		return &ErrUnconfirmedOrder{ClientOrderID: request.ClientOrderID, Err: fmt.Errorf(
			"発注は受理されました（注文番号 %s）が台帳の更新に失敗しました: %w", stringOf(ack.BrokerOrderID), uerr)}
	}
	intent(execution.ReasonPlaced, "")
	return nil
}

// placeResolving は送って結果が分からなければ当日の注文一覧で判定し、届いていなければ
// 種を変えて 1 度だけ送り直す。返す request は最後に送ったもの。
//
// 判定できなかった（一覧を照会できない・まだ載っていない）ときは PENDING のまま
// ErrUnconfirmedOrder を返す。次の実行（cron は 3 分おきに 3 回）の冒頭で
// ResolvePending がもう一度判定する。人は介在しない。
func placeResolving(env Env, b broker.Broker, build func(attempt int) domain.OrderRequest, attempt int,
	price decimal.Decimal, fee *decimal.Decimal) (domain.OrderRequest, error) {
	request := build(attempt)
	err := PlaceRecorded(env, b, request, price, fee)
	var unconfirmed *ErrUnconfirmedOrder
	if !errors.As(err, &unconfirmed) {
		return request, err
	}
	if !env.boundedWait(env.RetryWait) {
		// 締め切りで待ちを縮めた。受付が一覧に載る前に見ると「届いていない」と誤読して
		// 種を変えて送り直す（二重発注）ので、判定は次の実行の冒頭（猶予つき）に回す
		env.Report.Warn("daytrade.pending_unresolved", "締め切りで一覧の反映を待てないので送信結果不明の注文は次の実行で判定する",
			map[string]any{"client_order_id": request.ClientOrderID, "retry_wait": env.RetryWait.String()})
		return request, err
	}
	if _, rerr := ResolvePending(env, b, 0); rerr != nil {
		env.Report.Warn("daytrade.pending_unresolved", "送信結果不明の注文を判定できません（次の実行で再判定）", map[string]any{
			"client_order_id": request.ClientOrderID, "error": rerr.Error()})
		return request, err
	}
	current, ok, _ := env.Ledger.Get(request.ClientOrderID)
	switch {
	case !ok || current.Status == string(domain.OrderStatusPending):
		return request, err
	case current.Status == string(domain.OrderStatusUnsent):
		env.Report.Info("daytrade.pending_resolved", "届いていなかったので種を変えて送り直す", map[string]any{
			"client_order_id": request.ClientOrderID, "symbol": request.Symbol, "attempt": attempt + 1})
		request = build(attempt + 1)
		return request, PlaceRecorded(env, b, request, price, fee)
	default:
		env.Report.Info("daytrade.pending_resolved", "届いていた（注文番号を台帳に帰属）", map[string]any{
			"client_order_id": request.ClientOrderID, "symbol": request.Symbol,
			"broker_order_id": stringOf(current.BrokerOrderID), "status": current.Status})
		return request, nil
	}
}

// ResolvePending は台帳の PENDING（送信結果不明）を、ブローカーの当日の注文一覧と
// 突き合わせて判定し、台帳を更新する（wbcore/reconcile）。
//
//   - 届いていた   → 注文番号と状態を書き戻す
//   - 届いていない → UNSENT（終了状態。次の判断で種を変えて送り直せる）
//   - 決められない → PENDING のまま残し、Error ログと通知（AI が読む）
//
// 一覧を照会できなければエラー。判定できないまま実弾を出さない。
//
// 対象は台帳の日（day）ではなく**発注時刻が今日（JST）**の PENDING。持ち越しの返済は
// 建てた日の下に記録されるので、day で引くと翌日以降に送った返済の PENDING を永久に
// 拾えず、その脚が止まったままになる。立花の一覧は当日分しか返らないので、前の日に
// 送った PENDING は判定できない（警告だけ残す）。
//
// 今日送って注文番号まで分かっている注文が一覧に 1 つも無ければ、一覧が空で返った・
// 反映が遅れていると読み、「該当なし」を「届いていない」とはしない（reconcile.Options.Expected）。
func ResolvePending(env Env, b broker.Broker, grace time.Duration) (reconcile.Summary, error) {
	var summary reconcile.Summary
	if b == nil {
		return summary, nil
	}
	open, err := env.Ledger.OpenOrders()
	if err != nil {
		return summary, err
	}
	now := clock.NowUTC()
	todayJST := clock.ToZone(now, clock.Tokyo)
	start := time.Date(todayJST.Year(), todayJST.Month(), todayJST.Day(), 0, 0, 0, 0, clock.Tokyo)
	end := start.AddDate(0, 0, 1)

	var pendings []reconcile.Pending
	days := map[string]string{} // client_order_id → 台帳の日（ログ用）
	var stale []string
	for _, o := range open {
		if o.Status != string(domain.OrderStatusPending) {
			continue
		}
		placedAt, perr := time.Parse(time.RFC3339, o.PlacedAt)
		if perr != nil || placedAt.Before(start) || !placedAt.Before(end) {
			stale = append(stale, o.ClientOrderID)
			continue
		}
		days[o.ClientOrderID] = o.Day.Format(cli.DateLayout)
		pendings = append(pendings, reconcile.Pending{
			ClientOrderID: o.ClientOrderID, Symbol: o.Symbol, Side: o.Side, Trade: o.Trade,
			Quantity: o.Quantity, PlacedAt: placedAt,
		})
	}
	if len(stale) > 0 {
		env.Report.Warn("daytrade.pending_unresolved", "今日より前に送った送信結果不明の注文は判定しない（一覧は当日分のみ）",
			map[string]any{"day": env.dayText(), "pending": len(stale), "client_order_ids": stale})
	}
	if len(pendings) == 0 {
		return summary, nil
	}
	todays, err := b.GetOrderHistory(start, end.Add(-time.Second))
	if err != nil {
		return summary, fmt.Errorf("送信結果不明の注文 %d 件を判定できません（当日の注文一覧を照会できない）: %w", len(pendings), err)
	}
	known, err := env.Ledger.BrokerOrderIDs()
	if err != nil {
		return summary, err
	}
	placedToday, err := env.Ledger.PlacedBetween(start, end)
	if err != nil {
		return summary, err
	}
	expected := map[string]struct{}{}
	for _, o := range placedToday {
		if o.IsDryRun() || o.BrokerOrderID == nil || *o.BrokerOrderID == "" {
			continue
		}
		expected[*o.BrokerOrderID] = struct{}{}
	}
	resolutions := reconcile.Resolve(pendings, todays, reconcile.Options{Now: now, Grace: grace, Known: known, Expected: expected})
	var ambiguous []string
	for _, r := range resolutions {
		fields := r.Fields()
		fields["day"] = days[r.Pending.ClientOrderID]
		switch r.Outcome {
		case reconcile.Attributed:
			m := r.Match
			if err := env.Ledger.UpdateStatus(r.Pending.ClientOrderID, m.Status, m.FilledQuantity, m.AvgFillPrice, m.BrokerOrderID); err != nil {
				return summary, fmt.Errorf("判定の結果を台帳に書けません: %w", err)
			}
			env.Report.Info("daytrade.pending_resolved", "送信結果不明の注文は届いていた", fields)
		case reconcile.NotSent:
			if err := env.Ledger.UpdateStatus(r.Pending.ClientOrderID, domain.OrderStatusUnsent, decimal.Zero, nil, nil); err != nil {
				return summary, fmt.Errorf("判定の結果を台帳に書けません: %w", err)
			}
			env.Report.Info("daytrade.pending_resolved", "送信結果不明の注文は届いていなかった（送り直せる）", fields)
		case reconcile.Ambiguous:
			fields["fix"] = strings.Replace(fields["fix"].(string), "<app>", "daytrade", 1)
			env.Report.Error("daytrade.pending_ambiguous", "送信結果不明の注文を決められない（PENDING のまま。この銘柄は今日は触らない）", fields)
			ambiguous = append(ambiguous, fmt.Sprintf("%s %s %s 株: %s", r.Pending.Symbol, r.Pending.Side, r.Pending.Quantity, r.Reason))
		case reconcile.TooRecent:
			env.Report.Info("daytrade.pending_resolved", "送った直後なので次の実行で判定する", fields)
		}
	}
	if len(ambiguous) > 0 {
		env.Report.Alert("デイトレ: 送信結果不明の注文を自動で決められません（口座の注文一覧を確かめてください）",
			strings.Join(ambiguous, "\n"))
	}
	return reconcile.Summarize(resolutions), nil
}

// PlacePicks は選んだ銘柄を順に発注する。b が nil なら dry-run（台帳に記録だけ）。
// 台帳への記録が先（PlaceRecorded）。余力は取引区分ごとに減らしていく。
func PlacePicks(env Env, b broker.Broker, picks []selection.Pick) (orders int, failures []string, err error) {
	// 余力は取引区分ごと（現物は買付余力、信用は新規建可能額）。同じ枠を使う注文で減らしていく
	remaining := map[domain.TradeType]decimal.Decimal{}
	// 送る直前の時価は**ループの前に 1 回で**取る。1 銘柄ずつだと注文の合間に往復が直列で入り、
	// 測ろうとしている「判断から発注までの遅れ」をこの照会自身が増やす
	env.RefPrices = prefetchRefPrices(env, b, picks)

	for _, pick := range picks {
		pick := pick
		attempt := env.Ledger.DeadCount(env.Day, pick.Symbol, pick.Side)
		build := func(a int) domain.OrderRequest { return EntryRequest(pick, env.Day, env.Cfg, a) }
		request := build(attempt)
		label := "買い"
		if pick.Side == domain.SideSell {
			label = "売建"
		}
		already, err := env.Ledger.WasPlaced(request.ClientOrderID)
		if err != nil {
			// 発注済みか分からないまま送ると二重発注になりうる。この実行は止める
			return orders, failures, err
		}
		if already {
			env.printf("  %s: %sは発注済み（冪等）\n", pick.Symbol, label)
			skipRow(pick, request, execution.ReasonIdempotent, "")
			continue
		}
		outcome := ""
		if b == nil {
			if err := env.Ledger.Record(request, env.Day, ledger.DryRunStatus, &pick.Price, nil); err != nil {
				return orders, failures, err
			}
			skipRow(pick, request, execution.ReasonDryRun, "")
			outcome = "dry-run"
			orders++
		} else if env.expired() {
			// 締め切りを過ぎた。ここから先の注文は送らない——時間帯の外に成行を出さないため。
			// 送れなかった分は次の cron が「残りの枚数」として建て直す
			outcome = fmt.Sprintf("見送り 締め切り（%s）を過ぎた", env.deadlineText())
			failures = append(failures, fmt.Sprintf("%s %s: %s", pick.Symbol, label, outcome))
			env.printf("  %s: %s\n", pick.Symbol, outcome)
			skipRow(pick, request, execution.ReasonWindowClosed, outcome)
		} else {
			if _, ok := remaining[request.Trade]; !ok {
				balance, err := b.GetBalance()
				if err != nil {
					// 余力が分からないのはこの 1 銘柄の見送りにとどめ、次の銘柄へ進む。
					// ここで実行ごと止めると、既に建てた銘柄があるとき残りが二度と建たない
					outcome = fmt.Sprintf("見送り 余力を照会できない: %v", err)
					failures = append(failures, fmt.Sprintf("%s %s: %s", pick.Symbol, label, outcome))
					env.printf("  %s: %s\n", pick.Symbol, outcome)
					env.Report.Warn("daytrade.balance_failed", "余力を照会できず見送り", map[string]any{
						"day": env.dayText(), "symbol": pick.Symbol, "trade": string(request.Trade), "error": err.Error(),
					})
					skipRow(pick, request, execution.ReasonBrokerError, outcome)
					logOrder(env, pick, request, b != nil, outcome)
					continue
				}
				remaining[request.Trade] = balance.BuyingPowerFor(request.Trade)
			}
			need := pick.Amount().Add(pick.Fee())
			if need.GreaterThan(remaining[request.Trade]) {
				outcome = fmt.Sprintf("見送り 余力不足（必要 %s / 余力 %s）", cli.Yen(need), cli.Yen(remaining[request.Trade]))
				failures = append(failures, fmt.Sprintf("%s %s: %s", pick.Symbol, label, outcome))
				env.printf("  %s: %s\n", pick.Symbol, outcome)
				skipRow(pick, request, execution.ReasonInsufficientFunds, outcome)
				logOrder(env, pick, request, b != nil, outcome)
				continue
			}
			remaining[request.Trade] = remaining[request.Trade].Sub(need)
			fee := pick.Fee()
			var err error
			if request, err = placeResolving(env, b, build, attempt, pick.Price, &fee); err != nil {
				outcome = fmt.Sprintf("失敗 %v", err)
				failures = append(failures, fmt.Sprintf("%s %s: %v", pick.Symbol, label, err))
				env.printf("  %s: %s\n", pick.Symbol, outcome)
			} else {
				outcome = "発注"
				orders++
			}
		}
		logOrder(env, pick, request, b != nil, outcome)
	}
	return orders, failures, nil
}

// logOrder は寄付の 1 注文の結末を残す（発注・見送り・失敗のどれでも 1 行）。
func logOrder(env Env, pick selection.Pick, request domain.OrderRequest, live bool, outcome string) {
	env.Report.Info("daytrade.order", "寄付の注文", map[string]any{
		"day": env.dayText(), "symbol": pick.Symbol,
		"side": string(pick.Side), "trade": string(request.Trade),
		"client_order_id": request.ClientOrderID,
		"quantity":        pick.Quantity.String(), "price": pick.Price.String(),
		"amount": pick.Amount().String(), "live": live, "outcome": outcome,
	})
}

// skipRow は発注しなかった建玉を実行品質に残す。
// **見送りの理由の分布が改善の材料になる。**
func skipRow(pick selection.Pick, request domain.OrderRequest, reason execution.ReasonCode, note string) {
	fee := pick.Fee()
	execution.Collect(execution.Spec{
		Event: execution.EventSkip, App: "daytrade",
		Symbol: pick.Symbol, Side: string(request.Side), Trade: string(request.Trade),
		ClientOrderID: request.ClientOrderID, Live: false,
		Quantity: pick.Quantity, IntentPrice: pick.Price,
		IntentAmount: pick.Amount(), IntentFee: fee,
		Reason: reason, Note: note,
	})
}

// EnsureNoUnrecordedPositions は、これから建てる銘柄に台帳外の建玉が無いか確かめる。
//
// 台帳（client_order_id）に基づく冪等性は台帳が生きている前提の仕組みで、失うと
// 効かない。ブローカー側の建玉は失われないので、発注の直前にそちらと突き合わせる。
//
// 見つかったら**発注を中止する**。ここに残るのは現物だけで（信用は settleCarried が
// 返済する）、現物は積立の保有かもしれないので自動では手仕舞わない。人が確かめるまで
// 止める。信用でしか建てない構成（long_via_margin = true）なら現物は見ないので、
// 実際に止まるのは照会できないときだけになる。
//
// 照会できないときも中止する。二重建てを否定できないまま実弾を出さない。
//
// carried は今朝までに判定した持ち越しと、返済に回した台帳外の信用建玉。台帳が説明
// できる建玉なので差し引く——差し引かないと、持ち越した銘柄が今日も候補になった朝に
// 発注が丸ごと止まる。
//
// held は実行の冒頭（持ち越しの判定）で照会した建玉をそのまま使う。その後に送ったのは
// 持ち越しの返済だけで、それが約定していれば建玉は減る方向なので、古い照会で判定しても
// 結果は同じか保守側にしかならない。照会し直すと 1 実行で 2 電文増える。
func EnsureNoUnrecordedPositions(env Env, held broker.LegPositions, picks []selection.Pick, carried []Carried) error {
	if len(picks) == 0 {
		return nil
	}

	// 台帳が知っている今日の建玉ぶんと持ち越しぶんは差し引く（正常な再実行では止めない）
	recorded, err := recordedByLeg(env, carried)
	if err != nil {
		return err
	}

	var unrecorded []string
	seen := map[string]struct{}{}
	for _, pick := range picks {
		if _, done := seen[pick.Symbol]; done {
			continue
		}
		seen[pick.Symbol] = struct{}{}
		for _, leg := range CheckedLegs(pick.Symbol, env.Cfg) {
			position, ok := held.At(leg)
			if !ok {
				// 見る側を照会できていない。二重建てを否定できないまま実弾を出さない
				what := "現物"
				if leg.Margin {
					what = "信用建玉"
				}
				env.Report.Error("daytrade.unrecorded_check_failed", what+"を照会できません",
					map[string]any{"day": env.dayText(), "error": held.Err(leg.Margin).Error()})
				return fmt.Errorf("%sを照会できないため発注を中止しました（二重に建てないため）: %w",
					what, held.Err(leg.Margin))
			}
			leftover := position.Quantity.Sub(recorded[leg])
			if !leftover.IsPositive() {
				continue
			}
			unrecorded = append(unrecorded, fmt.Sprintf("%s %s %s 株（台帳の記録は %s 株）",
				pick.Symbol, LegName(leg), leftover, recorded[leg]))
		}
	}
	if len(unrecorded) == 0 {
		return nil
	}

	sort.Strings(unrecorded)
	env.Report.Error("daytrade.unrecorded_positions", "台帳に無い建玉があります",
		map[string]any{"day": env.dayText(), "positions": unrecorded})
	env.Report.Alert("デイトレ: 台帳に無い建玉があります（二重に建てる恐れ）。発注を中止しました",
		strings.Join(unrecorded, "、"))
	return &ErrUnrecordedPositions{Positions: unrecorded, LedgerPath: env.Ledger.Path()}
}

// ---------------------------------------------------------------------------
// 引けの手仕舞い
// ---------------------------------------------------------------------------

// ExitTarget は手仕舞う 1 建玉。
type ExitTarget struct {
	Entry     ledger.Order
	Quantity  decimal.Decimal
	FillPrice *decimal.Decimal
	// Unrecorded は台帳が説明できない建玉か（Entry は建玉から組み立てた作り物）。
	// client_order_id の種を分けるために持つ——台帳の建玉の手仕舞いと同じ銘柄・
	// 同じ株数になると ID が衝突し、片方が「発注済み（冪等）」として送られない。
	Unrecorded bool
}

// LiveEntries は今日の建玉のうち dry-run でないもの。dryRun は除いた数。
func LiveEntries(env Env) (entries []ledger.Order, dryRun int, err error) {
	all, err := env.Ledger.EntriesOn(env.Day)
	if err != nil {
		return nil, 0, err
	}
	for _, o := range all {
		if o.IsDryRun() {
			dryRun++
			continue
		}
		entries = append(entries, o)
	}
	return entries, dryRun, nil
}

// exitState は同じ銘柄・脚の今日の手仕舞い。
type exitState struct {
	// done は生きている（未確定の）か全部約定した手仕舞いがある——重ねて出さない。
	// FILLED は台帳に約定数量が入っていないことがあるので、数量ではなく状態で見る。
	done bool
	// filled は一部だけ約定して終わった（取消・失効）手仕舞いの約定数量の合計。
	filled decimal.Decimal
}

// placedExits は今日の手仕舞い（銘柄|脚 → 状態）。約定 0 で終わった拒否・失効は数えない。
func placedExits(env Env) (map[string]exitState, error) {
	all, err := env.Ledger.ExitsOn(env.Day)
	if err != nil {
		return nil, err
	}
	exits := map[string]exitState{}
	for _, o := range all {
		if o.IsDryRun() || o.IsDead() {
			continue
		}
		key := o.Symbol + "|" + o.Leg()
		s := exits[key]
		if o.IsOpen() || o.Status == string(domain.OrderStatusFilled) {
			s.done = true
		} else {
			s.filled = s.filled.Add(o.FilledQuantity)
		}
		exits[key] = s
	}
	return exits, nil
}

// remainingToExit は建玉の約定数量 filled のうち、まだ手仕舞っていない株数。
// 手仕舞いが生きている・全部約定しているなら 0。
func remainingToExit(exits map[string]exitState, order ledger.Order, filled decimal.Decimal) decimal.Decimal {
	s, ok := exits[order.Symbol+"|"+order.Leg()]
	if !ok {
		return filled
	}
	if s.done {
		return decimal.Zero
	}
	return decimal.Max(filled.Sub(s.filled), decimal.Zero)
}

// RefreshEntries は建玉の約定数量をブローカーに聞き、手仕舞う対象を組む。
//
// 聞けなかったときに台帳の値（発注直後は 0）へ黙って落ちてはいけない。
// 0 は「手仕舞う数量なし」として扱われるので、建玉があっても売らずに
// 終わり、そのまま持ち越しになる。確かめられなかった銘柄は unconfirmed に積む。
//
// b が nil（dry-run）なら送信済み・送信中を全約定とみなして対象を示す。
func RefreshEntries(env Env, b broker.Broker, entries []ledger.Order) (targets []ExitTarget, unconfirmed []string, err error) {
	exits, err := placedExits(env)
	if err != nil {
		return nil, nil, err
	}
	for _, order := range entries {
		filled := order.FilledQuantity
		fillPrice := order.AvgFillPrice
		// 台帳で確定済み（約定・拒否・未送信・失効）の注文はブローカーに聞かず台帳の値を使う
		// （queryFill と同じ）。もう変わらないうえ、未送信・拒否は注文番号が無く、聞くと
		// 毎回エラーの警告になる（night-repair の雑音）
		if b != nil && order.IsOpen() {
			current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
			if err != nil {
				env.printf("  %s: 照会に失敗: %v\n", order.Symbol, err)
				current = nil
			}
			if current == nil && filled.LessThanOrEqual(decimal.Zero) &&
				domain.OrderStatus(order.Status).IsOpen() {
				// 建てたかどうかが分からない。数量を推測して売ると、
				// 建っていなかった場合に新規の反対建玉を作ってしまう。
				unconfirmed = append(unconfirmed,
					fmt.Sprintf("%s（%s / %s 株）", order.Symbol, order.Status, order.Quantity))
				env.Report.Warn("daytrade.unconfirmed", "買い注文の約定を確かめられません", map[string]any{
					"day": env.dayText(), "symbol": order.Symbol,
					"client_order_id": order.ClientOrderID, "status": order.Status,
				})
				continue
			}
			if current != nil {
				filled, fillPrice = current.FilledQuantity, current.AvgFillPrice
				fillReason := execution.ReasonExpired
				if filled.GreaterThan(decimal.Zero) {
					fillReason = execution.ReasonFilled
				}
				execution.Collect(execution.Spec{
					Event: execution.EventFill, App: "daytrade",
					Symbol: order.Symbol, Side: string(order.Side), Trade: string(order.Trade),
					ClientOrderID: order.ClientOrderID,
					BrokerOrderID: stringOf(current.BrokerOrderID),
					Live:          true, Quantity: order.Quantity,
					IntentPrice:  decimalOrNil(order.Price),
					RefPrice:     decimalOrNil(order.RefPrice),
					FillQuantity: filled, FillPrice: decimalOrNil(fillPrice),
					Reason: fillReason,
				})
				recordFill(env, order, current, filled, fillPrice, "買い注文の約定状況")
			} else {
				// 台帳に約定が残っている（照会は空でも過去に確定済み）。その値で手仕舞う
				env.Report.Warn("daytrade.fill", "買い注文を照会できず台帳の確定値で続行", map[string]any{
					"day": env.dayText(), "symbol": order.Symbol,
					"client_order_id": order.ClientOrderID, "filled": filled.String(),
				})
			}
		} else if b == nil && filled.IsZero() &&
			(order.Status == string(domain.OrderStatusSubmitted) || order.Status == string(domain.OrderStatusPending)) {
			filled = order.Quantity // dry-run では全約定とみなして対象を示す
		}
		if filled.LessThanOrEqual(decimal.Zero) {
			env.printf("  %s: 約定なし（%s）。手仕舞う数量がありません\n", order.Symbol, order.Status)
			continue
		}
		remaining := remainingToExit(exits, order, filled)
		if remaining.LessThanOrEqual(decimal.Zero) {
			env.printf("  %s: 手仕舞い発注済み（冪等）\n", order.Symbol)
			continue
		}
		if !remaining.Equal(filled) {
			env.printf("  %s: 手仕舞いのうち %s 株は約定して終わっている。残り %s 株を手仕舞う\n",
				order.Symbol, filled.Sub(remaining), remaining)
		}
		targets = append(targets, ExitTarget{Entry: order, Quantity: remaining, FillPrice: fillPrice})
	}
	return targets, unconfirmed, nil
}

// recordFill は照会の結果を台帳とログに残す。台帳に書けなくても手仕舞いは続ける
// （数量はもう手元にある）が、黙らない。
func recordFill(env Env, order ledger.Order, current *domain.Order, filled decimal.Decimal, fillPrice *decimal.Decimal, msg string) {
	if err := env.Ledger.UpdateStatus(order.ClientOrderID, current.Status, filled, fillPrice, current.BrokerOrderID); err != nil {
		env.Report.Error("daytrade.ledger", "約定状況を台帳に書けません", map[string]any{
			"day": env.dayText(), "symbol": order.Symbol,
			"client_order_id": order.ClientOrderID, "error": err.Error(),
		})
	}
	env.Report.Info("daytrade.fill", msg, map[string]any{
		"day": env.dayText(), "symbol": order.Symbol,
		"side": string(order.Side), "trade": string(order.Trade),
		"client_order_id": order.ClientOrderID,
		"before":          order.Status, "after": string(current.Status),
		"quantity": order.Quantity.String(), "filled": filled.String(),
	})
}

// fillResult は 1 注文の約定状況（queryFill の結果）。
type fillResult struct {
	Filled decimal.Decimal
	Price  *decimal.Decimal
	// Unconfirmed は結果が分からない——台帳で未確定なのに、ブローカーに該当が無いか照会に失敗した。
	// Err はその理由。該当が無いだけなら nil。
	Unconfirmed bool
	Err         error
	// Open は照会の後もまだ終わっていない（送信済み・一部約定で板に残っている）。
	Open bool
}

// queryFill は注文の約定数量と平均単価を出す。
//
// 台帳で確定済み（FILLED・失効・拒否など）の注文は**ブローカーに聞かない**。確定した注文は
// もう変わらないし、持ち越しの判定は 14 暦日ぶんの注文を open / close のたびに見るので、
// 全部を聞くと 1 実行で百を超える電文になる（立花は 1 件 1 電文）。未確定のものだけ照会し、
// 結果を台帳に残す。未確定なのに照会できなければ Unconfirmed——数量を推測しない。
func queryFill(env Env, b broker.Broker, order ledger.Order, msg string) fillResult {
	result := fillResult{Filled: order.FilledQuantity, Price: order.AvgFillPrice}
	if !order.IsOpen() {
		return result
	}
	current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
	if current == nil {
		result.Unconfirmed, result.Err = true, err
		return result
	}
	result.Filled, result.Price = current.FilledQuantity, current.AvgFillPrice
	result.Open = current.Status.IsOpen()
	recordFill(env, order, current, result.Filled, result.Price, msg)
	return result
}

// ExitRequest は 1 建玉の反対売買。信用で建てたものは返済、現物の買いは売却。
// action は人向けの動詞（売り／返済売り／返済買い）。
func ExitRequest(entry ledger.Order, quantity decimal.Decimal, day time.Time, cfg config.Config, attempt int) (domain.OrderRequest, string) {
	return ExitRequestAs(ExitTarget{Entry: entry, Quantity: quantity}, day, cfg, attempt, "引けで手仕舞い")
}

// ExitRequestAs は理由の言葉を変えた ExitRequest（持ち越しの返済は「翌寄りで持ち越しを手仕舞い」）。
// client_order_id の種は言葉では変わらない（同じ日・同じ試行なら同じ注文）。台帳外の
// 建玉の返済だけは種を分ける（ExitTarget.Unrecorded）。
func ExitRequestAs(target ExitTarget, day time.Time, cfg config.Config, attempt int, phrase string) (domain.OrderRequest, string) {
	entry, quantity := target.Entry, target.Quantity
	exitSide := domain.SideSell
	if entry.Side != domain.SideBuy {
		exitSide = domain.SideBuy
	}
	exitTrade := domain.TradeTypeCash
	if entry.Trade == domain.TradeTypeMarginOpen {
		exitTrade = domain.TradeTypeMarginClose
	}
	action := map[string]string{
		"SELL|CASH":         "売り",
		"SELL|MARGIN_CLOSE": "返済売り",
		"BUY|MARGIN_CLOSE":  "返済買い",
		"BUY|CASH":          "買い",
	}[string(exitSide)+"|"+string(exitTrade)]

	// 前回の手仕舞いが拒否されていたら種を変えて送り直す（同じ ID はブローカーが弾く）
	kind := "daytrade-close"
	if target.Unrecorded {
		kind = "daytrade-sweep"
	}
	seed := fmt.Sprintf("%s|%s|%d", kind, day.Format(cli.DateLayout), attempt)
	return domain.OrderRequest{
		ClientOrderID: domain.MakeClientOrderID(seed, entry.Symbol, exitSide, quantity),
		Symbol:        entry.Symbol,
		Side:          exitSide,
		OrderType:     domain.OrderTypeMarket,
		Quantity:      quantity,
		TaxType:       cfg.Execution.TaxAccountType,
		Reason: fmt.Sprintf("%s %s %s（%s）",
			cfg.StrategyName(), day.Format(cli.DateLayout), phrase, action),
		Trade: exitTrade,
	}, action
}

// PlaceExit は 1 建玉の反対売買を送る。b が nil なら dry-run。
func PlaceExit(env Env, b broker.Broker, target ExitTarget) (string, error) {
	return PlaceExitAs(env, b, target, "引けで手仕舞い")
}

// PlaceExitAs は理由の言葉を変えた PlaceExit（持ち越しの返済用）。
func PlaceExitAs(env Env, b broker.Broker, target ExitTarget, phrase string) (string, error) {
	entry := target.Entry
	exitSide := domain.SideSell
	if entry.Side != domain.SideBuy {
		exitSide = domain.SideBuy
	}
	attempt := env.Ledger.DeadCount(env.Day, entry.Symbol, exitSide)
	build := func(a int) domain.OrderRequest {
		req, _ := ExitRequestAs(target, env.Day, env.Cfg, a, phrase)
		return req
	}
	request, action := ExitRequestAs(target, env.Day, env.Cfg, attempt, phrase)
	already, err := env.Ledger.WasPlaced(request.ClientOrderID)
	if err != nil {
		// 発注済みか分からないまま送ると二重の返済（反対建玉）になりうる
		return "", err
	}
	if already {
		env.printf("  %s: %s発注済み（冪等）\n", entry.Symbol, action)
		return "冪等", nil
	}
	if b == nil {
		if err := env.Ledger.Record(request, env.Day, ledger.DryRunStatus, target.FillPrice, nil); err != nil {
			return "", err
		}
		env.printf("  %s: %s %s 株 dry-run\n", entry.Symbol, action, cli.Yen(target.Quantity))
		return "dry-run", nil
	}
	price := decimal.Zero
	if target.FillPrice != nil {
		price = *target.FillPrice
	}
	if env.expired() {
		// 時間帯の外に成行を出さない。手仕舞えなかった建玉は呼び出し側が持ち越しとして知らせる
		return "", fmt.Errorf("締め切り（%s）を過ぎたため%sを送りませんでした", env.deadlineText(), action)
	}
	if _, err := placeResolving(env, b, build, attempt, price, nil); err != nil {
		return "", err
	}
	env.printf("  %s: %s %s 株 発注\n", entry.Symbol, action, cli.Yen(target.Quantity))
	return "発注", nil
}

// PlaceExits は手仕舞いを順に送り、通らなかったものを返す。
func PlaceExits(env Env, b broker.Broker, targets []ExitTarget) (failures []string) {
	for _, target := range targets {
		outcome, err := PlaceExit(env, b, target)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", target.Entry.Symbol, err))
			env.printf("  %s: 失敗 %v\n", target.Entry.Symbol, err)
			outcome = "失敗"
		}
		env.Report.Info("daytrade.order", "引けの手仕舞い注文", map[string]any{
			"day": env.dayText(), "symbol": target.Entry.Symbol,
			"quantity": target.Quantity.String(), "live": b != nil, "outcome": outcome,
		})
	}
	return failures
}

// ---------------------------------------------------------------------------
// 引け後の検証
// ---------------------------------------------------------------------------

// VerifyResult は照会の突き合わせ。
type VerifyResult struct {
	// Carried は手仕舞えていない建玉（持ち越し）。
	Carried []string
	// Unconfirmed は照会できなかった注文。空でなければ「持ち越しなし」とは言えない。
	Unconfirmed []string
}

// LiveOrders は今日の本発注（dry-run を除く）を建玉と手仕舞いに分けて返す。
func LiveOrders(env Env) (entries, exits []ledger.Order, err error) {
	if entries, err = livePart(env.Ledger.EntriesOn(env.Day)); err != nil {
		return nil, nil, err
	}
	if exits, err = livePart(env.Ledger.ExitsOn(env.Day)); err != nil {
		return nil, nil, err
	}
	return entries, exits, nil
}

func livePart(orders []ledger.Order, err error) ([]ledger.Order, error) {
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Order, 0, len(orders))
	for _, o := range orders {
		if !o.IsDryRun() {
			out = append(out, o)
		}
	}
	return out, nil
}

// Verify は今日の建玉と手仕舞いをブローカーに照会し、脚ごとに突き合わせる。
// 台帳と食い違う建玉（送信結果不明の注文が実は通っていた等）もブローカー側で確かめる。
func Verify(env Env, b broker.Broker, entries, exits []ledger.Order) VerifyResult {
	var result VerifyResult
	noteUnconfirmed := func(order ledger.Order, err error) {
		reason := "応答に該当の注文がありません"
		if err != nil {
			reason = err.Error()
		}
		result.Unconfirmed = append(result.Unconfirmed, fmt.Sprintf("%s %s: %s", order.Symbol, order.Leg(), reason))
		env.Report.Warn("daytrade.unconfirmed", "注文を照会できません", map[string]any{
			"day": env.dayText(), "symbol": order.Symbol,
			"client_order_id": order.ClientOrderID, "error": reason,
		})
	}

	// 確定済みの注文は台帳の値を使い、未確定のものだけブローカーに聞く（queryFill）
	tally := func(orders []ledger.Order, msg string) map[string]decimal.Decimal {
		totals := map[string]decimal.Decimal{}
		for _, order := range orders {
			key := order.Symbol + "|" + order.Leg()
			fill := queryFill(env, b, order, msg)
			if fill.Unconfirmed {
				noteUnconfirmed(order, fill.Err)
			}
			totals[key] = totals[key].Add(fill.Filled)
		}
		return totals
	}
	opened := tally(entries, "建玉注文の約定状況")
	closed := tally(exits, "手仕舞い注文の約定状況")

	// 脚ごとの売買区分（現物か信用か）。ブローカーの建玉と突き合わせるときの鍵に使う
	entryTrade := map[string]domain.TradeType{}
	for _, order := range entries {
		key := order.Symbol + "|" + order.Leg()
		if _, seen := entryTrade[key]; !seen {
			entryTrade[key] = order.Trade
		}
	}

	flagged := map[string]struct{}{}
	keys := make([]string, 0, len(opened))
	for key := range opened {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		symbol, leg, _ := strings.Cut(key, "|")
		what := "買い"
		if leg == "short" {
			what = "売建"
		}
		remaining := opened[key].Sub(closed[key])
		switch {
		case remaining.GreaterThan(decimal.Zero):
			result.Carried = append(result.Carried, fmt.Sprintf("%s %s %s 株", symbol, what, cli.Yen(remaining)))
			flagged[symbol] = struct{}{}
			env.printf("  %s: %s %s 株が手仕舞えていません（持ち越し）\n", symbol, what, cli.Yen(remaining))
		case opened[key].GreaterThan(decimal.Zero):
			env.printf("  %s: %s %s 株 手仕舞い済み\n", symbol, what, cli.Yen(opened[key]))
		}
	}

	// 突合は脚ごと（現物 / 信用、買建 / 売建）。銘柄コードだけで数えると、積立が現物で
	// 持っている銘柄をデイトレが売建てた日に相殺されて、残った売建が見えなくなる
	held := broker.PositionsByLeg(b)
	for _, key := range keys {
		symbol, leg, _ := strings.Cut(key, "|")
		if _, already := flagged[symbol]; already {
			continue
		}
		// 見る側だけを問題にする。信用の脚の突合に現物の照会は要らない
		positionLeg := broker.LegOf(symbol, entryTrade[key], leg == "short")
		position, ok := held.At(positionLeg)
		if !ok {
			err := held.Err(positionLeg.Margin)
			env.Report.Warn("daytrade.reconcile", "建玉を照会できず突合を省略",
				map[string]any{"symbol": symbol, "leg": leg, "error": err.Error()})
			result.Unconfirmed = append(result.Unconfirmed,
				fmt.Sprintf("%s %s: 建玉の照会に失敗: %v", symbol, leg, err))
			continue
		}
		quantity := position.Quantity
		if !quantity.IsPositive() {
			continue
		}
		result.Carried = append(result.Carried, fmt.Sprintf("%s %s 株（台帳と不一致）", symbol, cli.Yen(quantity)))
		env.printf("  %s: ブローカーに %s 株の建玉（台帳では手仕舞い済み）\n", symbol, cli.Yen(quantity))
		env.Report.Error("daytrade.reconcile", "台帳と建玉が不一致", map[string]any{
			"day": env.dayText(), "symbol": symbol, "leg": leg,
			"held": quantity.String(),
		})
	}
	return result
}

func stringOf(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func decimalOrNil(v *decimal.Decimal) any {
	if v == nil {
		return nil
	}
	return *v
}
