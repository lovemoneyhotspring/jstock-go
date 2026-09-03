// Package evaluate はスクリーニングの妥当性——**選んだ銘柄は、選ばなかった銘柄より
// 上がったか**——を後から確かめる。
//
// 比べる相手が要るのが要点。採用した銘柄（adopted）の平均リターンだけを見ても、
// 相場全体が上がった日なら当然プラスになる。**同じ日に候補には挙がったが採用
// しなかった銘柄**（passed / rest）と並べて初めて、順位付けが効いているかが分かる。
//
//	adopted  上位 max_positions 件。次のサイクルで建てる候補
//	passed   閾値は超えたが採用枠から溢れた
//	rest     閾値未満
//
// 判断そのものは wbjp screen が state/wbjp/history/screen/ に積んでいる。
// ここはそれに後日の足を当てるだけで、**判断のロジックには一切触れない**。
package evaluate

import (
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// Kind は評価の結果を積む履歴の種類名。
const Kind = "evaluation"

// Groups は群の並び順（表示と集計で共通）。
var Groups = []string{"adopted", "passed", "rest"}

// EvaluationColumns は評価の履歴の列。
var EvaluationColumns = []corehistory.Column{
	// 判断のときの値（screen の履歴から写す）
	{Name: "symbol", Type: corehistory.TypeString},
	{Name: "rank", Type: corehistory.TypeInt64},
	{Name: "score", Type: corehistory.TypeFloat64},
	{Name: "group", Type: corehistory.TypeString},
	{Name: "entry_close", Type: corehistory.TypeFloat64},
	// 実績
	{Name: "horizon", Type: corehistory.TypeInt64},
	{Name: "exit_date", Type: corehistory.TypeDate},
	{Name: "exit_close", Type: corehistory.TypeFloat64},
	{Name: "ret_bp", Type: corehistory.TypeFloat64},
	// 判断の材料になった screen の実行（ログと突き合わせる鍵）
	{Name: "screen_run_id", Type: corehistory.TypeString},
}

// SummaryColumns は群ごとの集計の列。
var SummaryColumns = []corehistory.Column{
	{Name: "group", Type: corehistory.TypeString},
	{Name: "count", Type: corehistory.TypeInt64},
	{Name: "avg_ret_bp", Type: corehistory.TypeFloat64},
	{Name: "win_rate", Type: corehistory.TypeFloat64},
}

// ReviewColumns は日ごとの振り返りの列。
var ReviewColumns = []corehistory.Column{
	{Name: "day", Type: corehistory.TypeDate},
	{Name: "horizon", Type: corehistory.TypeInt64},
	{Name: "adopted", Type: corehistory.TypeInt64},
	{Name: "adopted_bp", Type: corehistory.TypeFloat64},
	{Name: "passed_bp", Type: corehistory.TypeFloat64},
	{Name: "rest_bp", Type: corehistory.TypeFloat64},
}

// TotalsColumns は期間ぶんの合計の列。
var TotalsColumns = []corehistory.Column{
	{Name: "days", Type: corehistory.TypeInt64},
	{Name: "avg_adopted_bp", Type: corehistory.TypeFloat64},
	{Name: "avg_rest_bp", Type: corehistory.TypeFloat64},
	{Name: "beat_rest_rate", Type: corehistory.TypeFloat64},
}

// BarReader は評価に要る足だけを読む（*data.BarStore が満たす）。
// テストで差し替えられるよう、必要な操作だけを切り出している。
type BarReader interface {
	Has(symbol string) bool
	Read(symbol, start, end string) ([]domain.Bar, error)
}

var _ BarReader = (*data.BarStore)(nil)

// GroupOf は screen の履歴 1 行がどの群かを返す。
func GroupOf(row map[string]any) string {
	if asBool(row["adopted"]) {
		return "adopted"
	}
	if asBool(row["passed"]) {
		return "passed"
	}
	return "rest"
}

// ForwardClose は entryDay から horizon 本先の足（日付と終値）。足りなければ ok=false。
//
// 暦日ではなく**足の本数**で数える。休場を挟んでも「何営業日後」の意味が
// 変わらないようにするため。
func ForwardClose(bars []domain.Bar, entryDay time.Time, horizon int) (time.Time, float64, bool) {
	if horizon <= 0 {
		return time.Time{}, 0, false
	}
	entry := entryDay.UTC().Format("2006-01-02")

	after := make([]domain.Bar, 0, len(bars))
	for _, bar := range bars {
		if bar.Date > entry {
			after = append(after, bar)
		}
	}
	sort.SliceStable(after, func(i, j int) bool { return after[i].Date < after[j].Date })
	if len(after) < horizon {
		return time.Time{}, 0, false
	}
	target := after[horizon-1]
	day, err := time.ParseInLocation("2006-01-02", target.Date, time.UTC)
	if err != nil {
		return time.Time{}, 0, false
	}
	return day, target.Close.InexactFloat64(), true
}

// Evaluate は 1 日ぶんのスクリーニング結果に、horizon 営業日後の終値を当てる。
//
// 足が届いていない銘柄（新しすぎる判断・上場廃止）は**行を落とさず**実績だけ
// null にする。落とすと「評価できた銘柄だけ」の偏った集計になるため。
func Evaluate(screens corehistory.Frame, store BarReader, day time.Time, horizon int) corehistory.Frame {
	cache := map[string][]domain.Bar{}
	rows := make([]map[string]any, 0, screens.Height())

	for _, screen := range screens.Rows {
		symbol, _ := screen["symbol"].(string)
		bars, cached := cache[symbol]
		if !cached {
			if store != nil && symbol != "" && store.Has(symbol) {
				bars, _ = store.Read(symbol, "", "")
			}
			cache[symbol] = bars
		}

		entry := asFloat(screen["close"])
		var exitDate, exitClose, retBP any
		if len(bars) > 0 && entry != nil && *entry != 0 {
			if found, price, ok := ForwardClose(bars, day, horizon); ok {
				exitDate = found
				exitClose = price
				retBP = (price/(*entry) - 1) * 10000
			}
		}

		rows = append(rows, map[string]any{
			"symbol":        symbol,
			"rank":          corehistory.ToInt(screen["rank"]),
			"score":         corehistory.ToFloat(screen["score"]),
			"group":         GroupOf(screen),
			"entry_close":   floatOrNil(entry),
			"horizon":       int64(horizon),
			"exit_date":     exitDate,
			"exit_close":    exitClose,
			"ret_bp":        retBP,
			"screen_run_id": screen["run_id"],
		})
	}

	// rank の無い行（履歴が古い等）は末尾に寄せる。Frame.SortBy は nil を最小と
	// 見なすので、ここでは自前で並べる
	sort.SliceStable(rows, func(i, j int) bool {
		a, aok := rows[i]["rank"].(int64)
		b, bok := rows[j]["rank"].(int64)
		if aok != bok {
			return aok
		}
		return aok && a < b
	})
	return corehistory.NewFrame(EvaluationColumns, rows)
}

// Scored は実績の出た行だけ。
//
// **履歴が 1 件も無いと列自体が無い**空フレームが来るので、列の有無から確かめる。
func Scored(evaluation corehistory.Frame) corehistory.Frame {
	if !evaluation.Has("ret_bp") {
		return corehistory.NewFrame(evaluation.Columns, nil)
	}
	return evaluation.Filter(func(row map[string]any) bool {
		return asFloat(row["ret_bp"]) != nil
	})
}

// Summarize は群ごとの件数・平均リターン・勝率。実績が出ていない行は除いて数える。
func Summarize(evaluation corehistory.Frame) corehistory.Frame {
	scored := Scored(evaluation)
	rows := []map[string]any{}
	if scored.IsEmpty() {
		return corehistory.NewFrame(SummaryColumns, rows)
	}

	type bucket struct {
		count, won int
		total      float64
	}
	buckets := map[string]*bucket{}
	seen := []string{}
	for _, row := range scored.Rows {
		group, _ := row["group"].(string)
		if buckets[group] == nil {
			buckets[group] = &bucket{}
			seen = append(seen, group)
		}
		value := *asFloat(row["ret_bp"])
		buckets[group].count++
		buckets[group].total += value
		if value > 0 {
			buckets[group].won++
		}
	}

	for _, group := range orderGroups(seen) {
		b := buckets[group]
		rows = append(rows, map[string]any{
			"group":      group,
			"count":      int64(b.count),
			"avg_ret_bp": b.total / float64(b.count),
			"win_rate":   float64(b.won) / float64(b.count),
		})
	}
	return corehistory.NewFrame(SummaryColumns, rows)
}

// orderGroups は Groups の順に並べ、未知の群は末尾に回す。
func orderGroups(seen []string) []string {
	rank := map[string]int{}
	for i, name := range Groups {
		rank[name] = i
	}
	out := append([]string{}, seen...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := rank[out[i]]
		b, bok := rank[out[j]]
		if !aok {
			a = len(Groups)
		}
		if !bok {
			b = len(Groups)
		}
		return a < b
	})
	return out
}

// Review は日ごとに adopted / passed / rest の平均リターンを横に並べる。
//
// adopted_bp が rest_bp を上回る日が多いほど、順位付けが効いている。
func Review(evaluations corehistory.Frame) corehistory.Frame {
	scored := Scored(evaluations)
	rows := []map[string]any{}
	if scored.IsEmpty() {
		return corehistory.NewFrame(ReviewColumns, rows)
	}

	type key struct {
		day     time.Time
		horizon int64
	}
	type agg struct {
		adopted int64
		sums    map[string]float64
		counts  map[string]int
	}
	order := []key{}
	byKey := map[key]*agg{}

	for _, row := range scored.Rows {
		day, _ := row["day"].(time.Time)
		horizon, _ := corehistory.ToInt(row["horizon"]).(int64)
		k := key{day: day, horizon: horizon}
		if byKey[k] == nil {
			byKey[k] = &agg{sums: map[string]float64{}, counts: map[string]int{}}
			order = append(order, k)
		}
		group, _ := row["group"].(string)
		if group == "adopted" {
			byKey[k].adopted++
		}
		byKey[k].sums[group] += *asFloat(row["ret_bp"])
		byKey[k].counts[group]++
	}

	sort.SliceStable(order, func(i, j int) bool { return order[i].day.Before(order[j].day) })
	for _, k := range order {
		a := byKey[k]
		rows = append(rows, map[string]any{
			"day":        k.day,
			"horizon":    k.horizon,
			"adopted":    a.adopted,
			"adopted_bp": mean(a.sums["adopted"], a.counts["adopted"]),
			"passed_bp":  mean(a.sums["passed"], a.counts["passed"]),
			"rest_bp":    mean(a.sums["rest"], a.counts["rest"]),
		})
	}
	return corehistory.NewFrame(ReviewColumns, rows)
}

// ReviewTotals は期間ぶんの合計。beat_rest_rate が、規則が効いているかの目安。
func ReviewTotals(table corehistory.Frame) corehistory.Frame {
	rows := []map[string]any{}
	if table.IsEmpty() {
		return corehistory.NewFrame(TotalsColumns, rows)
	}

	var adoptedSum, restSum float64
	var adoptedCount, restCount, beat int
	for _, row := range table.Rows {
		adopted := asFloat(row["adopted_bp"])
		rest := asFloat(row["rest_bp"])
		if adopted != nil {
			adoptedSum += *adopted
			adoptedCount++
		}
		if rest != nil {
			restSum += *rest
			restCount++
		}
		// 片方でも欠けている日は「上回った」と数えない（比較できていないため）
		if adopted != nil && rest != nil && *adopted > *rest {
			beat++
		}
	}

	rows = append(rows, map[string]any{
		"days":           int64(table.Height()),
		"avg_adopted_bp": mean(adoptedSum, adoptedCount),
		"avg_rest_bp":    mean(restSum, restCount),
		"beat_rest_rate": float64(beat) / float64(table.Height()),
	})
	return corehistory.NewFrame(TotalsColumns, rows)
}

func mean(total float64, count int) any {
	if count == 0 {
		return nil
	}
	return total / float64(count)
}

func floatOrNil(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// asFloat は履歴の値を float64 にする（欠損なら nil）。
func asFloat(value any) *float64 {
	converted := corehistory.ToFloat(value)
	if converted == nil {
		return nil
	}
	f := converted.(float64)
	return &f
}

func asBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int64:
		return v != 0
	default:
		return false
	}
}
