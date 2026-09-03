package evaluate

import (
	"slices"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// SummarySchema は Summarize の表の形。
var SummarySchema = []history.Column{
	{Name: "side", Type: history.TypeString},
	{Name: "rank_group", Type: history.TypeString},
	{Name: "count", Type: history.TypeInt64},
	{Name: "avg_net_bp", Type: history.TypeFloat64},
	{Name: "win_rate", Type: history.TypeFloat64},
	{Name: "hypo_pnl", Type: history.TypeFloat64},
	{Name: "actual_pnl", Type: history.TypeFloat64},
	{Name: "traded", Type: history.TypeInt64},
}

// ReviewSchema は日 × 脚の表。
var ReviewSchema = []history.Column{
	{Name: "day", Type: history.TypeDate},
	{Name: "side", Type: history.TypeString},
	{Name: "source", Type: history.TypeString},
	{Name: "picked_n", Type: history.TypeInt64},
	{Name: "picked_bp", Type: history.TypeFloat64},
	{Name: "next_bp", Type: history.TypeFloat64},
	{Name: "all_bp", Type: history.TypeFloat64},
	{Name: "picked_pnl", Type: history.TypeFloat64},
	{Name: "actual_pnl", Type: history.TypeFloat64},
	{Name: "traded", Type: history.TypeInt64},
	{Name: "candidates", Type: history.TypeInt64},
}

// ReviewTotalsSchema は期間の合計。
var ReviewTotalsSchema = []history.Column{
	{Name: "side", Type: history.TypeString},
	{Name: "days", Type: history.TypeInt64},
	{Name: "picked_bp", Type: history.TypeFloat64},
	{Name: "next_bp", Type: history.TypeFloat64},
	{Name: "all_bp", Type: history.TypeFloat64},
	{Name: "picked_win_days", Type: history.TypeFloat64},
	{Name: "beat_all_days", Type: history.TypeFloat64},
	{Name: "picked_pnl", Type: history.TypeFloat64},
	{Name: "actual_pnl", Type: history.TypeFloat64},
	{Name: "traded", Type: history.TypeInt64},
}

// agg は集計の途中の値。
type agg struct {
	count     int64
	netBP     []float64
	wins      int64
	hypoPnL   float64
	actualPnL float64
	hasActual bool
	traded    int64
}

func (a *agg) add(netBP float64, hypoPnL, actualPnL *float64, traded bool) {
	a.count++
	a.netBP = append(a.netBP, netBP)
	if netBP > 0 {
		a.wins++
	}
	if hypoPnL != nil {
		a.hypoPnL += *hypoPnL
	}
	if actualPnL != nil {
		a.actualPnL += *actualPnL
		a.hasActual = true
	}
	if traded {
		a.traded++
	}
}

// Summarize は脚 × 群（picked / next / rest）ごとの件数・平均 net bp・勝率・損益。
func Summarize(evaluation history.Frame) history.Frame {
	if evaluation.Height() == 0 {
		return history.NewFrame(SummarySchema, nil)
	}
	groups := map[string]*agg{}
	var order []string
	for _, row := range evaluation.Rows {
		netBP := floatPtrOf(row["net_bp"])
		if netBP == nil {
			continue // 日足を当てられない候補は集計に入れない
		}
		key := str(row["side"]) + "|" + str(row["rank_group"])
		if _, ok := groups[key]; !ok {
			groups[key] = &agg{}
			order = append(order, key)
		}
		groups[key].add(*netBP, floatPtrOf(row["hypo_pnl"]), floatPtrOf(row["actual_pnl"]), boolOf(row["traded"]))
	}
	slices.SortFunc(order, func(a, b string) int {
		sa, ga := splitKey(a)
		sb, gb := splitKey(b)
		if sa != sb {
			return compareString(sa, sb)
		}
		return slices.Index(RankGroups, ga) - slices.Index(RankGroups, gb)
	})
	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		side, group := splitKey(key)
		a := groups[key]
		rows = append(rows, map[string]any{
			"side": side, "rank_group": group, "count": a.count,
			"avg_net_bp": mean(a.netBP),
			"win_rate":   float64(a.wins) / float64(a.count),
			"hypo_pnl":   a.hypoPnL,
			"actual_pnl": actualOrNil(a),
			"traded":     a.traded,
		})
	}
	return history.NewFrame(SummarySchema, rows)
}

// LatestPerDay は同じ日に何度も evaluate していれば最後の 1 回だけを残す。
func LatestPerDay(evaluations history.Frame) history.Frame {
	if evaluations.Height() == 0 {
		return evaluations
	}
	latest := map[string]time.Time{}
	for _, row := range evaluations.Rows {
		day := dayKey(row["day"])
		at := timeOf(row["recorded_at"])
		if cur, ok := latest[day]; !ok || at.After(cur) {
			latest[day] = at
		}
	}
	return evaluations.Filter(func(row map[string]any) bool {
		return timeOf(row["recorded_at"]).Equal(latest[dayKey(row["day"])])
	})
}

// Review は日 × 脚ごとに「選んだ N」「次点」「候補全体」の平均 net bp と損益を並べる。
//
// 選定が妥当なら、picked ≥ next ≥ all の順に並ぶ日が多いはず。逆が続くなら順位付けの
// 規則（ギャップの小さい順／大きい順）がその相場で効いていない。
func Review(evaluations history.Frame) history.Frame {
	scored := LatestPerDay(evaluations)
	type bucket struct {
		day        time.Time
		side       string
		source     string
		pickedN    int64
		picked     []float64
		next       []float64
		all        []float64
		pickedPnL  float64
		actualPnL  float64
		hasActual  bool
		traded     int64
		candidates int64
	}
	buckets := map[string]*bucket{}
	var order []string
	for _, row := range scored.Rows {
		netBP := floatPtrOf(row["net_bp"])
		if netBP == nil {
			continue
		}
		day := timeOf(row["day"])
		side := str(row["side"])
		key := day.Format("2006-01-02") + "|" + side
		b, ok := buckets[key]
		if !ok {
			b = &bucket{day: day, side: side, source: str(row["ranking_source"])}
			buckets[key] = b
			order = append(order, key)
		}
		group := str(row["rank_group"])
		b.all = append(b.all, *netBP)
		b.candidates++
		switch group {
		case "picked":
			b.picked = append(b.picked, *netBP)
			b.pickedN++
			if v := floatPtrOf(row["hypo_pnl"]); v != nil {
				b.pickedPnL += *v
			}
		case "next":
			b.next = append(b.next, *netBP)
		}
		if v := floatPtrOf(row["actual_pnl"]); v != nil {
			b.actualPnL += *v
			b.hasActual = true
		}
		if boolOf(row["traded"]) {
			b.traded++
		}
	}
	slices.Sort(order)
	rows := make([]map[string]any, 0, len(order))
	for _, key := range order {
		b := buckets[key]
		actual := any(nil)
		if b.hasActual {
			actual = b.actualPnL
		}
		rows = append(rows, map[string]any{
			"day": b.day, "side": b.side, "source": b.source,
			"picked_n":  b.pickedN,
			"picked_bp": mean(b.picked), "next_bp": mean(b.next), "all_bp": mean(b.all),
			"picked_pnl": b.pickedPnL, "actual_pnl": actual,
			"traded": b.traded, "candidates": b.candidates,
		})
	}
	return history.NewFrame(ReviewSchema, rows)
}

// ReviewTotals は Review の表を脚ごとに合計・平均する。
func ReviewTotals(table history.Frame) history.Frame {
	if table.Height() == 0 {
		return history.NewFrame(ReviewTotalsSchema, nil)
	}
	type totals struct {
		days                    int64
		picked, next, all       []float64
		pickedWin, beatAll, cmp int64
		winDenom                int64
		pickedPnL, actualPnL    float64
		hasActual               bool
		traded                  int64
	}
	byLeg := map[string]*totals{}
	var order []string
	for _, row := range table.Rows {
		side := str(row["side"])
		t, ok := byLeg[side]
		if !ok {
			t = &totals{}
			byLeg[side] = t
			order = append(order, side)
		}
		t.days++
		pickedBP := floatPtrOf(row["picked_bp"])
		allBP := floatPtrOf(row["all_bp"])
		if pickedBP != nil {
			t.picked = append(t.picked, *pickedBP)
			t.winDenom++
			if *pickedBP > 0 {
				t.pickedWin++
			}
			if allBP != nil {
				t.cmp++
				if *pickedBP > *allBP {
					t.beatAll++
				}
			}
		}
		if v := floatPtrOf(row["next_bp"]); v != nil {
			t.next = append(t.next, *v)
		}
		if allBP != nil {
			t.all = append(t.all, *allBP)
		}
		if v := floatPtrOf(row["picked_pnl"]); v != nil {
			t.pickedPnL += *v
		}
		if v := floatPtrOf(row["actual_pnl"]); v != nil {
			t.actualPnL += *v
			t.hasActual = true
		}
		t.traded += intOf(row["traded"])
	}
	slices.Sort(order)
	rows := make([]map[string]any, 0, len(order))
	for _, side := range order {
		t := byLeg[side]
		actual := any(nil)
		if t.hasActual {
			actual = t.actualPnL
		}
		rows = append(rows, map[string]any{
			"side": side, "days": t.days,
			"picked_bp": mean(t.picked), "next_bp": mean(t.next), "all_bp": mean(t.all),
			"picked_win_days": ratio(t.pickedWin, t.winDenom),
			"beat_all_days":   ratio(t.beatAll, t.cmp),
			"picked_pnl":      t.pickedPnL, "actual_pnl": actual, "traded": t.traded,
		})
	}
	return history.NewFrame(ReviewTotalsSchema, rows)
}

func mean(values []float64) any {
	if len(values) == 0 {
		return nil
	}
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}

func ratio(numerator, denominator int64) any {
	if denominator == 0 {
		return nil
	}
	return float64(numerator) / float64(denominator)
}

func actualOrNil(a *agg) any {
	if !a.hasActual {
		return nil
	}
	return a.actualPnL
}

func splitKey(key string) (side, group string) {
	for i := range key {
		if key[i] == '|' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func timeOf(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t
	}
	return time.Time{}
}

func dayKey(v any) string { return timeOf(v).Format("2006-01-02") }
