// Package reconcile は「送信したが結果が分からない注文（PENDING）」を、
// ブローカーの当日の注文一覧と突き合わせて判定する。
//
// なぜ要るか:
// 通信断・タイムアウト・セッション失効（p_errno）では「届いていない」と
// 「届いたが応答が返らない」を区別できない。台帳には PENDING が残り、次の実行は
// WasPlaced で弾かれて送り直さない——二重発注は防げるが、その日は買い漏れになる。
// 誰も端末を見ていない運用では、これを人に回さずプログラムが決める必要がある。
//
// 立花は client_order_id を保持しないので、突き合わせは**銘柄・売買・取引区分・
// 数量・発注時刻**で行う。台帳が既に注文番号を知っている注文は候補から外す。
//
//   - 一致する注文が 1 つ以上ある → Attributed（届いていた）。注文番号と状態を台帳へ
//   - 同じ銘柄・売買の注文が無い   → NotSent（届いていない）。UNSENT にして送り直す
//   - 同じ銘柄・売買はあるが数量や区分が違う → Ambiguous（決められない）。止めて知らせる
//   - 送った直後（Grace 未満）      → TooRecent。一覧に反映されるまで待つ
//
// wbjp / accum / daytrade の 3 つの台帳が同じ判定器を使う。台帳の型が違うので、
// ここは domain の型だけで書き、台帳への書き戻しは各呼び出し側が行う。
package reconcile

import (
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// Pending は台帳の PENDING（送信結果不明）1 件。
type Pending struct {
	ClientOrderID string
	Symbol        string
	Side          domain.Side
	// Trade は空なら比べない（accum のように現物固定の台帳）。
	Trade    domain.TradeType
	Quantity decimal.Decimal
	PlacedAt time.Time
}

// Outcome は判定。
type Outcome string

const (
	// Attributed はブローカーに該当の注文があった（届いていた）。
	Attributed Outcome = "attributed"
	// NotSent はブローカーに無かった（届いていない）。送り直してよい。
	NotSent Outcome = "not_sent"
	// Ambiguous は同じ銘柄・売買の未帰属の注文があるが細部が違い、決められない。
	Ambiguous Outcome = "ambiguous"
	// TooRecent は送った直後で、一覧に反映される前かもしれない。判定を見送る。
	TooRecent Outcome = "too_recent"
)

// Resolution は 1 件の判定。
type Resolution struct {
	Pending Pending
	Outcome Outcome
	// Match は Attributed のときブローカー側の注文。
	Match  *domain.Order
	Reason string
}

// Options は判定の条件。
type Options struct {
	Now time.Time
	// Grace は PlacedAt からこれ未満なら TooRecent にする。
	Grace time.Duration
	// Known は台帳が既に知っている broker_order_id。候補から外す。
	Known map[string]struct{}
}

// DefaultGrace は送信からこれだけ経てば一覧に載っているとみなす時間。
const DefaultGrace = 5 * time.Second

// earliestCreatedBefore は発注時刻よりどれだけ前の注文まで候補にするか
// （時計のずれ・台帳の書き込みとブローカーの受付の順序の揺れ）。
const earliestCreatedBefore = 5 * time.Minute

// Resolve は pending の各注文を todays（ブローカーの当日の注文一覧）と突き合わせる。
// 1 つのブローカー注文は 1 つの PENDING にしか帰属させない。PlacedAt の古い順に決める。
func Resolve(pending []Pending, todays []domain.Order, opts Options) []Resolution {
	used := map[string]struct{}{}
	for id := range opts.Known {
		used[id] = struct{}{}
	}
	ordered := append([]Pending(nil), pending...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].PlacedAt.Before(ordered[j].PlacedAt) })

	out := make([]Resolution, 0, len(ordered))
	for _, p := range ordered {
		if opts.Grace > 0 && !p.PlacedAt.IsZero() && opts.Now.Sub(p.PlacedAt) < opts.Grace {
			out = append(out, Resolution{Pending: p, Outcome: TooRecent,
				Reason: fmt.Sprintf("送信から %s 未満", opts.Grace)})
			continue
		}
		var exact []domain.Order
		similar := 0
		for _, o := range todays {
			if o.BrokerOrderID == nil {
				continue
			}
			if _, taken := used[*o.BrokerOrderID]; taken {
				continue
			}
			if o.Symbol != p.Symbol || o.Side != p.Side {
				continue
			}
			if o.CreatedAt != nil && !p.PlacedAt.IsZero() && o.CreatedAt.Before(p.PlacedAt.Add(-earliestCreatedBefore)) {
				continue // 発注よりずっと前からある注文は別物
			}
			if !o.Quantity.Equal(p.Quantity) || (p.Trade != "" && o.Trade != "" && o.Trade != p.Trade) {
				similar++
				continue
			}
			exact = append(exact, o)
		}
		switch {
		case len(exact) > 0:
			match := closest(exact, p.PlacedAt)
			used[*match.BrokerOrderID] = struct{}{}
			out = append(out, Resolution{Pending: p, Outcome: Attributed, Match: &match,
				Reason: fmt.Sprintf("注文番号 %s（%s）", *match.BrokerOrderID, match.Status)})
		case similar > 0:
			out = append(out, Resolution{Pending: p, Outcome: Ambiguous,
				Reason: fmt.Sprintf("同じ銘柄・売買で数量か区分の違う未帰属の注文が %d 件", similar)})
		default:
			out = append(out, Resolution{Pending: p, Outcome: NotSent, Reason: "当日の注文一覧に該当なし"})
		}
	}
	return out
}

// closest は発注時刻に最も近い（時刻が無ければ先頭の）注文。
func closest(orders []domain.Order, placedAt time.Time) domain.Order {
	best := orders[0]
	if placedAt.IsZero() {
		return best
	}
	bestGap := time.Duration(-1)
	for _, o := range orders {
		if o.CreatedAt == nil {
			continue
		}
		gap := o.CreatedAt.Sub(placedAt)
		if gap < 0 {
			gap = -gap
		}
		if bestGap < 0 || gap < bestGap {
			best, bestGap = o, gap
		}
	}
	return best
}

// Summary は判定の集計。ダイジェストに載せる（AI が最初に読む）。
type Summary struct {
	Attributed int
	NotSent    int
	Ambiguous  int
	TooRecent  int
	// Details は人向けの 1 行ずつ。
	Details []string
}

// Summarize は Resolution の列を集計する。
func Summarize(resolutions []Resolution) Summary {
	var s Summary
	for _, r := range resolutions {
		switch r.Outcome {
		case Attributed:
			s.Attributed++
		case NotSent:
			s.NotSent++
		case Ambiguous:
			s.Ambiguous++
		case TooRecent:
			s.TooRecent++
		}
		s.Details = append(s.Details, fmt.Sprintf("%s %s %s 株 → %s（%s）",
			r.Pending.Symbol, r.Pending.Side, r.Pending.Quantity, r.Outcome, r.Reason))
	}
	return s
}

// Fields はダイジェスト用の平らな項目。
func (s Summary) Fields(prefix string) map[string]any {
	return map[string]any{
		prefix + "_attributed": s.Attributed,
		prefix + "_unsent":     s.NotSent,
		prefix + "_ambiguous":  s.Ambiguous,
		prefix + "_too_recent": s.TooRecent,
	}
}
