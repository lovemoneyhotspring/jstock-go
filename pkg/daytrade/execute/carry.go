package execute

import (
	"fmt"
	"sort"
	"strings"
	"time"

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
	return fmt.Sprintf("%s %s %s 株（%s 建て）", c.Target.Entry.Symbol, what, cli.Yen(c.Target.Quantity), c.Day.Format(cli.DateLayout))
}

// CarriedPositions は直近 CarryLookbackDays 暦日の台帳を遡り、手仕舞えていない建玉を集める。
//
// 残りは「建玉の約定 − 手仕舞いの約定」をブローカーに照会して出し、ブローカーの建玉と
// 突き合わせる。建玉が無ければ（手で返済済み）対象にしない。照会できなかった注文がある
// 銘柄は unconfirmed に積んで対象にしない——数量を推測して返済すると、建っていなかった
// 場合に新規の反対建玉を作る。
//
// 建玉を照会できなければ error。持ち越しの有無が分からないまま新規に建てると二重に
// なりうるので、呼び出し側は発注を止める（EnsureNoUnrecordedPositions と同じ判断）。
func CarriedPositions(env Env, b broker.Broker) (carried []Carried, unconfirmed []string, err error) {
	positions, err := broker.PositionsBySymbolIncludingMargin(b)
	if err != nil {
		env.Report.Error("daytrade.carry_check_failed", "建玉を照会できません",
			map[string]any{"day": env.dayText(), "error": err.Error()})
		return nil, nil, fmt.Errorf("建玉を照会できないため持ち越しを判定できません: %w", err)
	}

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

		opened := map[string]decimal.Decimal{}
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
				filled := order.FilledQuantity
				price := order.AvgFillPrice
				current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
				switch {
				case current != nil:
					filled, price = current.FilledQuantity, current.AvgFillPrice
					recordFill(envDay, order, current, filled, price, "持ち越しの照会")
				case order.IsOpen():
					// 結果が分からない注文。数量を推測しない
					reason := "応答に該当の注文がありません"
					if err != nil {
						reason = err.Error()
					}
					unconfirmed = append(unconfirmed, fmt.Sprintf("%s %s（%s）: %s",
						day.Format(cli.DateLayout), order.Symbol, order.Leg(), reason))
					blocked[key] = struct{}{}
				}
				totals[key] = totals[key].Add(filled)
				if isEntry {
					if _, seen := first[key]; !seen {
						first[key], fillPrice[key] = order, price
					}
				}
			}
			return totals
		}
		opened = tally(entries, true)
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
			held := positions[symbol].Quantity
			if leg == "short" {
				held = held.Neg()
			}
			if held.LessThanOrEqual(decimal.Zero) {
				env.printf("  %s: 台帳では %s 株が未返済だがブローカーに建玉が無い（手で返済済み）\n", symbol, cli.Yen(remaining))
				env.Report.Warn("daytrade.carry", "台帳の未返済がブローカーに無い", map[string]any{
					"day": day.Format(cli.DateLayout), "symbol": symbol, "leg": leg, "remaining": remaining.String(),
				})
				continue
			}
			if held.LessThan(remaining) {
				remaining = held
			}
			carried = append(carried, Carried{Day: day, Target: ExitTarget{
				Entry: first[key], Quantity: remaining, FillPrice: fillPrice[key],
			}})
		}
	}
	return carried, unconfirmed, nil
}

// ReturnCarried は持ち越しを成行で手仕舞う。phrase は台帳の理由に残す言葉（翌寄り／引け）。
//
// 台帳には建てた日の下に記録する。再実行は client_order_id で冪等。締め切りは env のもの
// （寄付なら 9:15、引けなら 15:30）を使う。通らなかったものを返す。
func ReturnCarried(env Env, b broker.Broker, carried []Carried, phrase string) (failures []string) {
	for _, c := range carried {
		envDay := env
		envDay.Day = c.Day
		outcome, err := PlaceExitAs(envDay, b, c.Target, phrase)
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
// 残りが 1 注文の予算以上なら件数を floor(残り ÷ 予算) に減らす（予算はそのまま）。
// 予算に満たなくても残りがあれば **1 件を残りの金額で**建てる——一部が拘束されただけで
// 一日を休むのは機会損失。0 件になるのは残りが無いときだけ。
func CapByTied(n int, capital, tied, budget decimal.Decimal) (int, decimal.Decimal) {
	if n <= 0 || budget.LessThanOrEqual(decimal.Zero) {
		return 0, budget
	}
	remaining := capital.Sub(tied)
	if remaining.LessThanOrEqual(decimal.Zero) {
		return 0, budget
	}
	if remaining.LessThan(budget) {
		return 1, remaining.Floor()
	}
	return min(n, int(remaining.Div(budget).Floor().IntPart())), budget
}

// carriedQuantities は持ち越しを銘柄 → 符号付き株数（買いは正、売建は負）にする。
// 台帳外建玉の検査で「台帳が知っている建玉」として差し引くため。
func carriedQuantities(carried []Carried) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	for _, c := range carried {
		q := c.Target.Quantity
		if c.Target.Entry.Side != domain.SideBuy {
			q = q.Neg()
		}
		out[c.Target.Entry.Symbol] = out[c.Target.Entry.Symbol].Add(q)
	}
	return out
}
