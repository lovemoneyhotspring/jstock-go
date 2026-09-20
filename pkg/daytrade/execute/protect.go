package execute

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// 保険の手仕舞い（daytrade protect）。
//
// 建てた玉に、執行条件「引け」の手仕舞い（返済・売り）を**ブローカーに先に置く**。注文は立花に
// 残るので、cron が止まった・マシンが止まった・ネットが切れたときも、引け（15:30 の板寄せ）で
// 手仕舞われる。人が気づいて端末を打つ前提にしない安全網。
//
// 引け値は 15:20 の成行より両脚とも不利（分足の検証で合算 +525 万 → +470 万）。だから保険は
// 「使われない」のが正常で、15:20 の close が保険を取り消してから成行で手仕舞う
// （RefreshEntries → ReleaseProtection）。取り消せたと確かめられないときは成行を出さず、
// 保険に任せる（返済できる建玉が保険の注文に押さえられているので、出しても通らない）。

// ProtectAction は 1 銘柄への保険の置き方の結果。
type ProtectAction struct {
	Symbol   string
	Quantity string
	// Outcome は 発注 / 冪等 / 失敗。
	Outcome string
	Err     error
}

// ProtectEntries は今日の建玉のうち約定した株数のぶん、まだ保険が置かれていない株数に保険の
// 手仕舞いを置く。何度呼んでも重ならない（置いた株数を差し引く）。b が nil（dry-run）なら何もしない。
//
// 保険が拒否された銘柄は、その日はもう置かない（毎回同じ理由で拒否され、通知が増えるだけ。
// 銘柄単位なので、その銘柄だけ通らない場合に他の銘柄の安全網まで止めない）。
// 置けなくても close は従来どおり 15:20 に手仕舞うので、売買は止めない。
func ProtectEntries(env Env, b broker.Broker) ([]ProtectAction, error) {
	if b == nil {
		return nil, nil
	}
	all, err := env.Ledger.ExitsOn(env.Day)
	if err != nil {
		return nil, err
	}
	rejected := map[string]bool{}
	for _, o := range all {
		if o.IsProtective() && !o.IsDryRun() && o.Status == string(domain.OrderStatusRejected) {
			rejected[o.Symbol] = true
		}
	}
	entries, _, err := LiveEntries(env)
	if err != nil {
		return nil, err
	}
	exits, err := placedExits(env, true)
	if err != nil {
		return nil, err
	}
	var actions []ProtectAction
	for _, order := range entries {
		if order.IsDead() {
			continue
		}
		if rejected[order.Symbol] {
			env.printf("  %s: 保険の引け注文は今日すでに拒否されている。置きません\n", order.Symbol)
			continue
		}
		fill := queryFill(env, b, order, "保険の手仕舞いの前の約定照会")
		if fill.Unconfirmed {
			// 建ったか分からない。数量を推測して置くと、建っていなければ新規の反対建玉になる
			env.printf("  %s: 約定を確かめられません。保険は次の回で\n", order.Symbol)
			continue
		}
		filled := fill.Filled
		if !order.IsOpen() {
			filled = settledQuantity(order)
		}
		if !filled.IsPositive() {
			continue
		}
		key := order.Symbol + "|" + order.Leg()
		remaining := remainingToExit(exits, order, filled)
		if !remaining.IsPositive() {
			continue
		}
		outcome, err := PlaceExitAs(env, b, ExitTarget{
			Entry: order, Quantity: remaining, FillPrice: fill.Price,
			AlreadyExited: exits[key], Protective: true,
		}, "引けの保険")
		act := ProtectAction{Symbol: order.Symbol, Quantity: remaining.String(), Outcome: outcome}
		if err != nil {
			act.Outcome, act.Err = "失敗", err
			env.printf("  %s: 保険の引け注文に失敗 %v\n", order.Symbol, err)
		} else {
			env.printf("  %s: 保険の引け注文 %s 株（%s）\n", order.Symbol, remaining, outcome)
		}
		env.Report.Info("daytrade.protect", "保険の引け注文", map[string]any{
			"day": env.dayText(), "symbol": order.Symbol, "quantity": remaining.String(), "outcome": act.Outcome,
		})
		actions = append(actions, act)
	}
	return actions, nil
}

// ReleaseProtection は今日の生きている保険の手仕舞いを取り消し、結末（約定数量）を台帳に残す。
// symbols が nil なら全銘柄、あればその銘柄だけ。b が nil（dry-run）なら何もしない。
//
// 取消は**全部に先に送ってから**終わりを確かめる（1 銘柄ずつ待つと、最初の成行が出るまでに
// 銘柄数 × 数秒かかる。成行は 15:20 の約定を狙うので遅らせたくない）。
//
// 取り消したと確かめられなかった保険は held（"銘柄|脚" → 理由）に返す。照会が空・エラー・
// 取消中のままは全部これに入る（生きているかもしれない注文）。同じ銘柄に保険が複数あって一部だけ
// 取り消せたときも、取り消せなかった保険は生きたまま数えられる（呼び出し側は残りの株数だけ出す）。
// 取り消せずに約定していた（引けの前に約定してしまった）保険は「手仕舞い済み」として台帳に残し、警告する。
// 通知は呼び出し側が出す（close は unconfirmed、guard は GuardAction.Err）。
func ReleaseProtection(env Env, b broker.Broker, symbols map[string]bool) (held map[string]string, err error) {
	held = map[string]string{}
	if b == nil {
		return held, nil
	}
	all, err := env.Ledger.ExitsOn(env.Day)
	if err != nil {
		return nil, err
	}
	type pending struct {
		order    ledger.Order
		current  *domain.Order
		brokerID *string
	}
	var open []pending
	// 1 周目: 照会して、生きているものに取消を送る
	for _, o := range all {
		if !o.IsProtective() || o.IsDryRun() || !o.IsOpen() {
			continue
		}
		if symbols != nil && !symbols[o.Symbol] {
			continue
		}
		key := o.Symbol + "|" + o.Leg()
		current, qerr := b.GetOrder(o.ClientOrderID, o.BrokerOrderID)
		if current == nil {
			reason := "応答に該当の注文がありません"
			if qerr != nil {
				reason = qerr.Error()
			}
			held[key] = "照会できません: " + reason
			continue
		}
		p := pending{order: o, current: current, brokerID: current.BrokerOrderID}
		if p.brokerID == nil {
			p.brokerID = o.BrokerOrderID
		}
		if !current.Status.IsTerminal() {
			if env.expired() {
				held[key] = fmt.Sprintf("締め切り（%s）を過ぎたため取消を送れません", env.deadlineText())
				continue
			}
			sendCancel(env, b, o, p.brokerID, "daytrade.protect_cancel")
		}
		open = append(open, p)
	}
	// 2 周目: 終わりを確かめて台帳に残す（最初の 1 本だけ待ち、あとは待たずに見る）
	var released, filledEarly []string
	for i, p := range open {
		o := p.order
		key := o.Symbol + "|" + o.Leg()
		current := pollOrder(env, b, o, p.current, p.brokerID, min(env.RetryWait, closeCancelWait), i == 0)
		recordFill(env, o, current, current.FilledQuantity, current.AvgFillPrice, "保険の引け注文の取消")
		switch {
		case !current.Status.IsTerminal():
			held[key] = fmt.Sprintf("取消の完了を確かめられません（%s）", current.Status)
		case current.Status == domain.OrderStatusCancelled && !current.FilledQuantity.IsPositive():
			released = append(released, o.Symbol)
		default:
			// 引けの前に約定・失効した。約定した株数は手仕舞い済みとして数える（placedExits）
			filledEarly = append(filledEarly, fmt.Sprintf("%s（%s / 約定 %s 株）", o.Symbol, current.Status, current.FilledQuantity))
		}
	}
	if len(released) > 0 {
		sort.Strings(released)
		env.printf("  保険の引け注文を取り消した: %s\n", strings.Join(released, " "))
	}
	if len(filledEarly) > 0 {
		sort.Strings(filledEarly)
		env.printf("  保険の引け注文が引けの前に終わっていた: %s\n", strings.Join(filledEarly, " "))
		env.Report.Warn("daytrade.protect_filled_early", "保険の引け注文が引けの前に約定・失効していた（実機の挙動を確かめる）",
			map[string]any{"day": env.dayText(), "orders": filledEarly})
		digest.Anomaly("daytrade.protect_filled_early", strings.Join(filledEarly, "、"))
	}
	if len(held) > 0 {
		lines := make([]string, 0, len(held))
		for key, reason := range held {
			lines = append(lines, key+": "+reason)
		}
		sort.Strings(lines)
		env.Report.Warn("daytrade.protect_held", "保険の引け注文を取り消せず、その株数は引け値の手仕舞いに任せる",
			map[string]any{"day": env.dayText(), "held": lines})
	}
	return held, nil
}
