package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 材料（TOB・MBO など）が後から判明した売建の処置（daytrade guard）。
//
// open は判定の時点の記録簿で材料の出た銘柄を外すが、記録簿の取り込み（8:52）の後や場中の公表は
// 知らないまま売建てる。買付価格までサヤ寄せしてストップ高に張り付くと、成行の売りは引けの
// ストップ配分で約定し、返済の買いは通らず持ち越す（2026-09-15 の 8848）。場中に記録簿を取り直し、
// 印の付いた今日の売建を次のように処置する:
//
//   - まだ終わっていない注文は取消を送り、終わる（取消完了・全部約定）まで照会し直す。取消は
//     非同期で、取消中にも約定は増える。最終の約定数量を台帳に書いてから返済の株数を決める
//   - 約定があれば、その株数を成行で返済買い（一部約定・全部約定とも）。種は引けの手仕舞いと同じ
//     （PlaceExitAs）なので、close が同じ株数を数えれば冪等で重ならない

// guardPolls は取消を送った後に「終わったか」を照会し直す回数。間隔は cancelOpen の引数
// （guard は Env.RetryWait、引けの close は closeCancelWait まで）。
const guardPolls = 5

// GuardAction は材料の出た売建 1 件への処置。
type GuardAction struct {
	Symbol        string
	ClientOrderID string
	// Kind は材料の種類（news.Kind*）。
	Kind     string
	Quantity decimal.Decimal
	// Filled は処置の時点で確定した約定数量。
	Filled decimal.Decimal
	// Cancelled は取消を送った。Returned は返済を送った株数（冪等で送らなかったら 0）。
	Cancelled bool
	Returned  decimal.Decimal
	// Result は人向けの結末。Err が非 nil なら処置を終えられていない（人が口座を見る）。
	Result string
	Err    error
}

// Acted は取消か返済を送ったか（通知の対象）。
func (a GuardAction) Acted() bool { return a.Cancelled || a.Returned.IsPositive() }

// GuardCorpEvents は今日の信用新規売りのうち、marks（銘柄 → 材料の種類）に載る銘柄を処置する。
// b が nil（dry-run）なら何を送るかを示すだけで、台帳もブローカーも触らない。
func GuardCorpEvents(env Env, b broker.Broker, marks map[string]string) ([]GuardAction, error) {
	entries, _, err := LiveEntries(env)
	if err != nil {
		return nil, err
	}
	var targets []ledger.Order
	for _, o := range entries {
		if _, ok := marks[o.Symbol]; !ok || !IsShortEntry(o) || o.IsDead() {
			continue
		}
		targets = append(targets, o)
	}
	// 返済の時価もまとめて取る。1 銘柄ずつだと材料の出た銘柄の数だけ往復が直列に入る
	// （PlaceExits にしか入れていなかった。2026-09-17 のレビュー）
	symbols := make([]string, 0, len(targets))
	for _, o := range targets {
		symbols = append(symbols, o.Symbol)
	}
	env.RefPrices, env.refBatchFailed = prefetchRefPrices(env, b, symbols)
	var actions []GuardAction
	for _, o := range targets {
		act := GuardAction{Symbol: o.Symbol, ClientOrderID: o.ClientOrderID, Kind: marks[o.Symbol], Quantity: o.Quantity}
		actions = append(actions, guardOne(env, b, o, act))
	}
	return actions, nil
}

// IsShortEntry は信用の新規売り（売建）か。
func IsShortEntry(o ledger.Order) bool {
	return o.Trade == domain.TradeTypeMarginOpen && o.Side == domain.SideSell
}

// GuardPending は材料の出た今日の売建のうち、処置が残っている銘柄——まだ終わっていない注文か、
// 約定があるのに返済が済んでいない（生きていない）もの。台帳だけで判定する。無ければ guard は
// ブローカーに接続しない（処置の済んだ売建で 10 分ごとにログインしない）。
func GuardPending(env Env, marks map[string]string) ([]string, error) {
	entries, _, err := LiveEntries(env)
	if err != nil {
		return nil, err
	}
	exits, err := placedExits(env)
	if err != nil {
		return nil, err
	}
	var pending []string
	for _, o := range entries {
		if _, ok := marks[o.Symbol]; !ok || !IsShortEntry(o) || o.IsDead() {
			continue
		}
		if o.IsOpen() || remainingToExit(exits, o, settledQuantity(o)).IsPositive() {
			pending = append(pending, o.Symbol)
		}
	}
	return pending, nil
}

// settledQuantity は終わった注文の建っている株数。状態が FILLED なのに約定数量が入っていない行は
// 注文数量とみなす（0 と読むと、全部約定した建玉を「建っていない」と数え、返済しない・台帳外として掃除する）。
func settledQuantity(o ledger.Order) decimal.Decimal {
	if o.Status == string(domain.OrderStatusFilled) && !o.FilledQuantity.IsPositive() {
		return o.Quantity
	}
	return o.FilledQuantity
}

// cancelOpen は板に残っている注文 o（current は直前の照会の結果で、まだ終わっていない）を取り消し、
// 終わるまで wait おきに guardPolls 回まで照会し直す。最後に見た状態と「取り消したと言えるか」を返す。
// 返った状態がまだ終わっていなければ、取消の完了を確かめられなかった（呼ぶ側が決める）。
// code は取消がエラーだったときの警告のコード。
func cancelOpen(env Env, b broker.Broker, o ledger.Order, current *domain.Order, wait time.Duration, code string) (*domain.Order, bool) {
	brokerID := current.BrokerOrderID
	if brokerID == nil {
		brokerID = o.BrokerOrderID
	}
	cancelErr := b.Cancel(o.ClientOrderID, brokerID)
	if cancelErr != nil {
		// 取消の間に全部約定した・すでに取消中など。照会し直して結末で決める
		env.Report.Warn(code, "取消がエラー。照会し直して決める", map[string]any{
			"day": env.dayText(), "symbol": o.Symbol, "client_order_id": o.ClientOrderID, "error": cancelErr.Error(),
		})
	}
	for i := 0; i < guardPolls && !current.Status.IsTerminal(); i++ {
		if !env.boundedWait(wait) && env.expired() {
			break
		}
		next, err := b.GetOrder(o.ClientOrderID, brokerID)
		if err != nil {
			env.printf("  %s: 取消の後の照会に失敗: %v\n", o.Symbol, err)
		}
		if next != nil {
			current = next
		}
	}
	// 取消が受け付けられたか、結末が取消のときだけ「取り消した」と言う（取消がエラーで
	// 実は全部約定していたなら、通知は返済だけにする）
	return current, cancelErr == nil || current.Status == domain.OrderStatusCancelled
}

func guardOne(env Env, b broker.Broker, o ledger.Order, act GuardAction) GuardAction {
	filled, price := settledQuantity(o), o.AvgFillPrice
	if o.IsOpen() {
		if b == nil {
			act.Result = fmt.Sprintf("dry-run: 未確定の売建 %s 株を取り消す（約定があれば返済する）", o.Quantity)
			return act
		}
		current, err := b.GetOrder(o.ClientOrderID, o.BrokerOrderID)
		if current == nil {
			// 終わったか分からない注文に取消も返済も出さない（反対建玉・二重の返済になりうる）
			if err == nil {
				err = fmt.Errorf("応答に該当の注文がありません")
			}
			act.Err = fmt.Errorf("売建を照会できません: %w", err)
			return act
		}
		if !current.Status.IsTerminal() {
			if env.expired() {
				act.Err = fmt.Errorf("締め切り（%s）を過ぎたため取消を送りませんでした", env.deadlineText())
				return act
			}
			current, act.Cancelled = cancelOpen(env, b, o, current, env.RetryWait, "daytrade.corp_guard")
		}
		recordFill(env, o, current, current.FilledQuantity, current.AvgFillPrice, "材料の出た売建の取消")
		o.Status, o.FilledQuantity, o.AvgFillPrice = string(current.Status), current.FilledQuantity, current.AvgFillPrice
		filled, price = settledQuantity(o), current.AvgFillPrice
		if !current.Status.IsTerminal() {
			act.Filled = filled
			act.Err = fmt.Errorf("取消の完了を確かめられません（%s、約定 %s 株）。次の回で照会し直します",
				current.Status, filled)
			return act
		}
	}
	act.Filled = filled
	if filled.LessThanOrEqual(decimal.Zero) {
		if act.Cancelled {
			act.Result = "取消（約定なし）"
		} else {
			act.Result = "約定なしで終わっていた"
		}
		return act
	}

	exits, err := placedExits(env)
	if err != nil {
		act.Err = err
		return act
	}
	remaining := remainingToExit(exits, o, filled)
	if remaining.LessThanOrEqual(decimal.Zero) {
		act.Result = fmt.Sprintf("約定 %s 株の返済は発注済み", filled)
		return act
	}
	if b == nil {
		act.Result = fmt.Sprintf("dry-run: 約定 %s 株のうち %s 株を返済買い", filled, remaining)
		return act
	}
	outcome, err := PlaceExitAs(env, b, ExitTarget{Entry: o, Quantity: remaining, FillPrice: price,
		AlreadyExited: exits[o.Symbol+"|"+o.Leg()]}, "材料（TOB など）で返済")
	if err != nil {
		act.Err = fmt.Errorf("約定 %s 株の返済を送れません: %w", remaining, err)
		return act
	}
	if outcome != "冪等" {
		act.Returned = remaining
	}
	prefix := ""
	if act.Cancelled {
		prefix = "残りを取消し、"
	}
	act.Result = fmt.Sprintf("%s約定 %s 株を返済買い（%s）", prefix, remaining, outcome)
	return act
}
