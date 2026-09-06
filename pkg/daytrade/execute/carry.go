package execute

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// CarryLookbackDays は持ち越しを探して台帳を遡る暦日数。何日も寄らない銘柄（連続ストップ高）と
// 連休を合わせても見失わない長さ。台帳の照会は日ごとに軽い。
const CarryLookbackDays = 14

// Carried は前営業日以前に建てて手仕舞えていない建玉（持ち越し）。
//
// 引けの返済が約定しない（ショートの引けストップ高、ロングのストップ安）と建玉が翌日に残る。
// 検証は margin.carry_penalty で「翌寄りで返済した」ことにしているので、実運用も同じにする。
type Carried struct {
	// Day は建てた日。台帳の記録はこの日の下に積む（その日の verify が手仕舞い済みになる）。
	Day time.Time
	// Target は手仕舞う対象。Quantity は残り（建玉の約定 − 手仕舞いの約定。ブローカーの建玉で頭打ち）。
	Target ExitTarget
}

// Leg は long / short。
func (c Carried) Leg() string { return c.Target.Entry.Leg() }

// Notional は拘束とみなす金額（残り株数 × 建値）。建値が無ければ 0。
//
// 信用の保証金は建玉の 3 割ほどだが、返済できていない玉は評価損も抱えているので
// 全額を拘束として数える（保守側）。
func (c Carried) Notional() decimal.Decimal {
	if c.Target.FillPrice == nil {
		return decimal.Zero
	}
	return c.Target.Quantity.Mul(*c.Target.FillPrice)
}

// String は人向け。
func (c Carried) String() string {
	what := "買い"
	if c.Leg() == "short" {
		what = "売建"
	}
	when := c.Day.Format(cli.DateLayout) + " 建て"
	if c.Target.Unrecorded {
		when = "台帳外"
	}
	return fmt.Sprintf("%s %s %s 株（%s）", c.Target.Entry.Symbol, what, cli.Yen(c.Target.Quantity), when)
}

// CarriedPositions は直近 CarryLookbackDays 暦日の台帳を遡り、手仕舞えていない建玉を集める。
//
// 残りは「建玉の約定 − 手仕舞いの約定」をブローカーに照会して出し、ブローカーの建玉と
// 突き合わせる。突合は**脚ごと**（現物 / 信用、買建 / 売建）に行う——銘柄コードだけで
// 数えると、積立が現物で持っている銘柄をデイトレが売建てた朝に相殺されて 0 になり、
// 売建の持ち越しを見失う。建玉が無ければ（手で返済済み）対象にしない。照会できなかった注文がある
// 銘柄は unconfirmed に積んで対象にしない——数量を推測して返済すると、建っていなかった
// 場合に新規の反対建玉を作る。
//
// **台帳が使っている側**（現物 / 信用）を照会できなければ error。持ち越しの有無が
// 分からないまま新規に建てると二重になりうるので、open は発注を止める
// （EnsureNoUnrecordedPositions と同じ判断）。使っていない側の障害では止めない——
// 信用でしか建てない構成なら、現物の照会が落ちてもデイトレの判断には要らない。
//
// held は照会済みの建玉（broker.PositionsByLeg）。呼び出し側が UnrecordedMargin と共有する。
// 注文の照会は台帳で未確定のものだけ（queryFill）。確定済みまで聞くと 14 暦日ぶんで
// 1 実行に百を超える電文になり、9:01 の発注より前にそれを消費してしまう。
func CarriedPositions(env Env, b broker.Broker, held broker.LegPositions) (carried []Carried, unconfirmed []string, err error) {
	// 同じ脚の持ち越しが複数日にあれば、ブローカーの建玉を日をまたいで消費する。
	// 日ごとに建玉全体を上限にすると、手で一部返済した後に返済の合計が建玉を超える
	consumed := map[broker.PositionLeg]decimal.Decimal{}

	for back := 1; back <= CarryLookbackDays; back++ {
		day := env.Day.AddDate(0, 0, -back)
		envDay := env
		envDay.Day = day
		entries, _, err := LiveEntries(envDay)
		if err != nil {
			return nil, nil, err
		}
		if len(entries) == 0 {
			continue
		}
		exits, err := envDay.Ledger.ExitsOn(day)
		if err != nil {
			return nil, nil, err
		}

		first := map[string]ledger.Order{}
		fillPrice := map[string]*decimal.Decimal{}
		blocked := map[string]struct{}{}
		tally := func(orders []ledger.Order, isEntry bool) map[string]decimal.Decimal {
			totals := map[string]decimal.Decimal{}
			for _, order := range orders {
				if order.IsDryRun() {
					continue
				}
				key := order.Symbol + "|" + order.Leg()
				fill := queryFill(envDay, b, order, "持ち越しの照会")
				if fill.Unconfirmed {
					// 結果が分からない注文。数量を推測しない
					reason := "応答に該当の注文がありません"
					if fill.Err != nil {
						reason = fill.Err.Error()
					}
					unconfirmed = append(unconfirmed, fmt.Sprintf("%s %s（%s）: %s",
						day.Format(cli.DateLayout), order.Symbol, order.Leg(), reason))
					blocked[key] = struct{}{}
				}
				totals[key] = totals[key].Add(fill.Filled)
				if isEntry {
					if _, seen := first[key]; !seen {
						first[key], fillPrice[key] = order, fill.Price
					}
				}
			}
			return totals
		}
		opened := tally(entries, true)
		closed := tally(exits, false)

		keys := make([]string, 0, len(opened))
		for key := range opened {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, skip := blocked[key]; skip {
				continue
			}
			remaining := opened[key].Sub(closed[key])
			if remaining.LessThanOrEqual(decimal.Zero) {
				continue
			}
			symbol, leg, _ := strings.Cut(key, "|")
			entry := first[key]
			positionLeg := broker.LegOf(symbol, entry.Trade, leg == "short")
			position, ok := held.At(positionLeg)
			if !ok {
				// この脚の建玉が照会できていない。0 株と読んで見送ると持ち越しを見失う
				return nil, nil, carryQueryError(env, held, positionLeg)
			}
			available := position.Quantity.Sub(consumed[positionLeg])
			if available.LessThanOrEqual(decimal.Zero) {
				env.printf("  %s: 台帳では %s 株が未返済だがブローカーに建玉が無い（手で返済済み）\n", symbol, cli.Yen(remaining))
				env.Report.Warn("daytrade.carry", "台帳の未返済がブローカーに無い", map[string]any{
					"day": day.Format(cli.DateLayout), "symbol": symbol, "leg": leg, "remaining": remaining.String(),
				})
				continue
			}
			if available.LessThan(remaining) {
				// 一部だけ手で返済された等。黙って切り詰めると気付けないので知らせる
				env.printf("  %s: 台帳の未返済 %s 株に対しブローカーの建玉は %s 株。少ない方を返済します\n",
					symbol, cli.Yen(remaining), cli.Yen(available))
				env.Report.Warn("daytrade.carry", "台帳の未返済がブローカーの建玉より多い", map[string]any{
					"day": day.Format(cli.DateLayout), "symbol": symbol, "leg": leg,
					"remaining": remaining.String(), "held": available.String(),
				})
				remaining = available
			}
			consumed[positionLeg] = consumed[positionLeg].Add(remaining)
			carried = append(carried, Carried{Day: day, Target: ExitTarget{
				Entry: entry, Quantity: remaining, FillPrice: fillPrice[key],
			}})
		}
	}
	return carried, unconfirmed, nil
}

// UnrecordedMargin は台帳が説明できない信用建玉を集める。持ち越しと同じ形で返すので、
// 呼び出し側は ReturnCarried でそのまま成行返済でき、拘束資金にも数えられる。
//
// **信用はデイトレでしか使わない**（積立は現物で買い増すだけ）。だから台帳外の信用建玉は
// 台帳が見失った自分の玉であって、他の戦略の保有ではない。台帳を失う・別ホストへ移す・
// 発注後に記録できずに落ちる、といったときに出る。放っておくと保証金を食い、翌日以降も
// 残るので、見つけた場に成行で返済する。
//
// 現物には触らない——積立の保有かもしれず、口座からは見分けられない。
//
// held は照会済みの建玉（CarriedPositions と同じもの）。carried は先に判定した持ち越し。
// 今日の台帳の建玉と合わせて差し引く（台帳が知っている建玉を二重に返済しない）。
// 返済注文を出したがまだ約定していない建玉も、台帳の建玉として差し引かれるので重複しない。
//
// 返済に回した銘柄はその日の候補から外す（SweptSymbols）。
func UnrecordedMargin(env Env, held broker.LegPositions, carried []Carried) ([]Carried, error) {
	// 見るのは信用だけ。現物の照会が落ちていてもデイトレの判断には要らない
	if err := held.MarginErr; err != nil {
		env.Report.Error("daytrade.sweep_check_failed", "信用建玉を照会できません",
			map[string]any{"day": env.dayText(), "error": err.Error()})
		return nil, fmt.Errorf("信用建玉を照会できないため台帳外の建玉を判定できません: %w", err)
	}
	recorded, err := recordedByLeg(env, carried)
	if err != nil {
		return nil, err
	}

	var out []Carried
	for _, leg := range held.Legs() {
		if !leg.Margin {
			continue // 現物は積立の保有かもしれないので触らない
		}
		position, _ := held.At(leg)
		leftover := position.Quantity.Sub(recorded[leg])
		if !leftover.IsPositive() {
			continue
		}
		side := domain.SideBuy
		if leg.Short {
			side = domain.SideSell
		}
		price := position.CostPrice
		env.printf("  %s: 台帳に無い%s %s 株。成行で返済します\n", leg.Symbol, LegName(leg), cli.Yen(leftover))
		env.Report.Error("daytrade.sweep", "台帳に無い信用建玉を返済", map[string]any{
			"day": env.dayText(), "symbol": leg.Symbol, "leg": LegName(leg),
			"quantity": leftover.String(), "held": position.Quantity.String(),
			"recorded": recorded[leg].String(),
		})
		out = append(out, Carried{Day: env.Day, Target: ExitTarget{
			// 建玉から組み立てた作り物。返済の向きと売買区分を決めるためだけに使う
			Entry: ledger.Order{
				Symbol: leg.Symbol, Side: side, Quantity: leftover,
				Trade: domain.TradeTypeMarginOpen,
			},
			Quantity:   leftover,
			FillPrice:  &price,
			Unrecorded: true,
		}})
	}
	return out, nil
}

// carryQueryError は必要な側の建玉を照会できなかったときのエラー。通知も出す。
// 側は脚（LegOf）で決める——TradeType.IsMargin とは空の扱いが違うので、At と同じ規則を使う。
func carryQueryError(env Env, held broker.LegPositions, leg broker.PositionLeg) error {
	what := "現物"
	if leg.Margin {
		what = "信用建玉"
	}
	err := held.Err(leg.Margin)
	env.Report.Error("daytrade.carry_check_failed", what+"を照会できません",
		map[string]any{"day": env.dayText(), "error": err.Error()})
	return fmt.Errorf("%sを照会できないため持ち越しを判定できません: %w", what, err)
}

// ReturnCarried は持ち越しを成行で手仕舞う。phrase は台帳の理由に残す言葉（翌寄り／引け）。
// 台帳外の建玉はそれと分かる言葉に差し替える。
//
// 台帳には建てた日の下に記録する。再実行は client_order_id で冪等。締め切りは env のもの
// （寄付なら 9:15、引けなら 15:30）を使う。通らなかったものを返す。
func ReturnCarried(env Env, b broker.Broker, carried []Carried, phrase string) (failures []string) {
	for _, c := range carried {
		envDay := env
		envDay.Day = c.Day
		reason := phrase
		if c.Target.Unrecorded {
			reason = "台帳に無い建玉を返済"
		}
		outcome, err := PlaceExitAs(envDay, b, c.Target, reason)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", c, err))
			env.printf("  %s: 失敗 %v\n", c.Target.Entry.Symbol, err)
			outcome = "失敗"
		}
		env.Report.Info("daytrade.order", "持ち越しの手仕舞い注文", map[string]any{
			"day": env.dayText(), "entry_day": c.Day.Format(cli.DateLayout), "symbol": c.Target.Entry.Symbol,
			"leg": c.Leg(), "quantity": c.Target.Quantity.String(), "live": b != nil, "outcome": outcome,
		})
	}
	return failures
}

// TiedCapital は持ち越しが拘束している金額を脚ごとに返す（ロング, ショート）。
func TiedCapital(carried []Carried) (long, short decimal.Decimal) {
	for _, c := range carried {
		if c.Leg() == "short" {
			short = short.Add(c.Notional())
		} else {
			long = long.Add(c.Notional())
		}
	}
	return long, short
}

// PositionsWithin は capital から tied を引いた残りで建てられる件数（1 件 budget）。0 以上。
func PositionsWithin(capital, tied, budget decimal.Decimal) int {
	if budget.LessThanOrEqual(decimal.Zero) {
		return 0
	}
	remaining := capital.Sub(tied)
	if remaining.LessThanOrEqual(decimal.Zero) {
		return 0
	}
	return int(remaining.Div(budget).Floor().IntPart())
}

// CapByTied は拘束資金を引いた残りで建てられる件数と 1 注文の予算。
//
// n は今日まだ建てられる件数（N − placed）、placed は今日すでに建てた件数。残りの資金は
// capital − tied − placed × budget——再実行は前の回が使った資金も引いて数える（引かないと、
// 途中で落ちた回の再実行が拘束を超えて建てる）。
// 残りが 1 注文の予算以上なら件数を floor(残り ÷ 予算) に減らす（予算はそのまま）。
// 予算に満たなくても残りがあれば **1 件を残りの金額で**建てる——一部が拘束されただけで
// 一日を休むのは機会損失。0 件になるのは残りが無いときだけ。
func CapByTied(n, placed int, capital, tied, budget decimal.Decimal) (int, decimal.Decimal) {
	if n <= 0 || budget.LessThanOrEqual(decimal.Zero) {
		return 0, budget
	}
	spent := budget.Mul(decimal.NewFromInt(int64(max(placed, 0))))
	remaining := capital.Sub(tied).Sub(spent)
	if remaining.LessThanOrEqual(decimal.Zero) {
		return 0, budget
	}
	if remaining.LessThan(budget) {
		return 1, remaining.Floor()
	}
	return min(n, int(remaining.Div(budget).Floor().IntPart())), budget
}

// carriedByLeg は持ち越しを脚 → 株数（常に正）にする。
// 台帳外建玉の検査で「台帳が知っている建玉」として差し引くため。
func carriedByLeg(carried []Carried) map[broker.PositionLeg]decimal.Decimal {
	out := map[broker.PositionLeg]decimal.Decimal{}
	for _, c := range carried {
		leg := broker.LegOf(c.Target.Entry.Symbol, c.Target.Entry.Trade, c.Leg() == "short")
		out[leg] = out[leg].Add(c.Target.Quantity)
	}
	return out
}

// recordedByLeg は台帳が知っている建玉を脚ごとに集める——判定済みの持ち越し（返済に回した
// 台帳外の建玉を含む）と、今日の建玉注文（生きている／約定した）。株数は注文数量（約定数量
// ではない）。建てた直後で約定数量がまだ台帳に無くても、ブローカーの建玉を台帳外と読まないため。
// UnrecordedMargin と EnsureNoUnrecordedPositions が同じ差し引きをする。
func recordedByLeg(env Env, carried []Carried) (map[broker.PositionLeg]decimal.Decimal, error) {
	recorded := carriedByLeg(carried)
	entries, err := env.Ledger.EntriesOn(env.Day)
	if err != nil {
		return nil, err
	}
	for _, o := range entries {
		if o.IsDryRun() || o.IsDead() {
			continue
		}
		leg := broker.LegOf(o.Symbol, o.Trade, o.Leg() == "short")
		recorded[leg] = recorded[leg].Add(o.Quantity)
	}
	return recorded, nil
}

// SweptSymbols は台帳外として返済に回した銘柄。その日の候補から外すために使う。
//
// 掃除の返済注文は建てた日が分からないので**今日の下**に記録される。同じ銘柄・同じ脚を
// 今日また建てると、引けの手仕舞いがその返済注文を「手仕舞い発注済み」と読んで当日の建玉を
// 返済せず、翌朝の持ち越し判定でも掃除の約定ぶんが差し引かれて残りが少なく出る。
// 台帳を失った朝にしか起きないので、その銘柄を一日休むほうが安い。
func SweptSymbols(carried []Carried) map[string]struct{} {
	out := map[string]struct{}{}
	for _, c := range carried {
		if c.Target.Unrecorded {
			out[c.Target.Entry.Symbol] = struct{}{}
		}
	}
	return out
}

// CheckedLegs は台帳外の建玉を探す脚。デイトレが作りうる脚だけを見る。
//
// 現物の買い玉は、long_via_margin のときは積立のものでしかありえない。
// これを台帳外として数えると、積立が持っている銘柄が候補に入った朝に
// 発注が丸ごと止まる（口座は共用、台帳は別なので照合しようがない）。
func CheckedLegs(symbol string, cfg config.Config) []broker.PositionLeg {
	legs := []broker.PositionLeg{
		{Symbol: symbol, Margin: true, Short: false},
		{Symbol: symbol, Margin: true, Short: true},
	}
	if EntryTrade(domain.SideBuy, cfg) == domain.TradeTypeCash {
		legs = append(legs, broker.PositionLeg{Symbol: symbol})
	}
	return legs
}

// LegName は人向けの脚の呼び名。
func LegName(leg broker.PositionLeg) string {
	switch {
	case !leg.Margin:
		return "現物"
	case leg.Short:
		return "売建"
	default:
		return "信用買い"
	}
}
