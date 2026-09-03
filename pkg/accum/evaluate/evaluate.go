// Package evaluate は積立の判断が効いたか——「増額した日は、本当に安かったのか」を測る。
//
// 積立は売らないので、スイングやデイトレのような「建てて外した損益」は無い。
// 代わりに問うのは倍率の当たり外れになる:
//
//	下落局面で倍率を上げて多く買った日の取得単価は、
//	その後の価格より安かったか（＝増額は報われたか）。
//
// 判断は state/accum/history/decision/ に積んである。ここではそれに horizon 営業日後の
// 終値を当て、倍率の帯ごとにその後のリターンを並べる。倍率 1.0 の日（通常の積立）が
// 対照群になる。
//
// accum backtest との違いは、こちらが実際に判断した記録を材料にすること。
// バックテストは規則を過去に当て直すが、こちらは運用が実際に見た値を使う。
package evaluate

import (
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// Kind は評価を積む履歴の種類。
const Kind = "evaluation"

// Buckets は倍率の帯。積立は 1.0 が通常で、下落局面ほど大きくなる。
var Buckets = []string{"1.0（通常）", "1.0〜1.5", "1.5〜2.0", "2.0 以上"}

// Columns は評価の履歴に書く列。
var Columns = []history.Column{
	{Name: "symbol", Type: history.TypeString},
	{Name: "judged_on", Type: history.TypeDate},
	{Name: "multiplier", Type: history.TypeFloat64},
	{Name: "bucket", Type: history.TypeString},
	{Name: "due", Type: history.TypeFloat64},
	{Name: "entry_close", Type: history.TypeFloat64},
	{Name: "horizon", Type: history.TypeInt64},
	{Name: "exit_date", Type: history.TypeDate},
	{Name: "exit_close", Type: history.TypeFloat64},
	{Name: "ret_bp", Type: history.TypeFloat64},
}

// SummaryColumns は帯ごとの集計の列。
var SummaryColumns = []history.Column{
	{Name: "bucket", Type: history.TypeString},
	{Name: "count", Type: history.TypeInt64},
	{Name: "avg_ret_bp", Type: history.TypeFloat64},
	{Name: "win_rate", Type: history.TypeFloat64},
	{Name: "due", Type: history.TypeFloat64},
}

// BucketOf は倍率の帯。境界は「通常か、増額か、どれだけ増額か」で切る。
func BucketOf(multiplier float64) string {
	switch {
	case multiplier <= 1.0:
		return Buckets[0]
	case multiplier < 1.5:
		return Buckets[1]
	case multiplier < 2.0:
		return Buckets[2]
	default:
		return Buckets[3]
	}
}

// ForwardClose は entryDay から horizon 本先の足（日付と終値）。足りなければ ok=false。
//
// 暦日ではなく足の本数で数える。休場を挟んでも「何営業日後」の意味が変わらないため。
func ForwardClose(bars []domain.Bar, entryDay string, horizon int) (string, float64, bool) {
	if horizon < 1 {
		return "", 0, false
	}
	seen := 0
	for _, b := range bars {
		if b.Date <= entryDay {
			continue
		}
		seen++
		if seen == horizon {
			c, _ := b.Close.Float64()
			return b.Date, c, true
		}
	}
	return "", 0, false
}

// Evaluate は判断の並びに horizon 営業日後の終値を当てる。
//
// 足が届いていない判断（新しすぎる）は行を落とさず実績だけ欠損にする。
// 落とすと「評価できた判断だけ」の偏った集計になる。
func Evaluate(decisions history.Frame, store *data.BarStore, horizon int) history.Frame {
	rows := make([]map[string]any, 0, decisions.Height())
	cache := map[string][]domain.Bar{}

	for _, row := range decisions.Rows {
		symbol, _ := row["symbol"].(string)
		bars, cached := cache[symbol]
		if !cached {
			if store != nil && store.Has(symbol) {
				bars, _ = store.Read(symbol, "", "")
			}
			cache[symbol] = bars
		}

		judged := dayOf(row["judged_on"])
		entry := floatOf(row["close"])

		var exitDate any
		var exitClose any
		var retBP any
		if len(bars) > 0 && judged != "" && entry != nil {
			if d, c, ok := ForwardClose(bars, judged, horizon); ok && *entry != 0 {
				exitDate = mustDay(d)
				exitClose = c
				retBP = (c/(*entry) - 1) * 10_000
			}
		}

		multiplier := 1.0
		if m := floatOf(row["multiplier"]); m != nil {
			multiplier = *m
		}

		var judgedValue any
		if judged != "" {
			judgedValue = mustDay(judged)
		}
		var entryValue any
		if entry != nil {
			entryValue = *entry
		}
		var dueValue any
		if d := floatOf(row["due"]); d != nil {
			dueValue = *d
		}

		rows = append(rows, map[string]any{
			"symbol":      symbol,
			"judged_on":   judgedValue,
			"multiplier":  multiplier,
			"bucket":      BucketOf(multiplier),
			"due":         dueValue,
			"entry_close": entryValue,
			"horizon":     int64(horizon),
			"exit_date":   exitDate,
			"exit_close":  exitClose,
			"ret_bp":      retBP,
		})
	}

	// 判定日の昇順。日付が欠けた行は末尾へ（並べ替えの基準が無いため）
	sort.SliceStable(rows, func(i, j int) bool {
		a, aok := rows[i]["judged_on"].(time.Time)
		b, bok := rows[j]["judged_on"].(time.Time)
		switch {
		case !aok:
			return false
		case !bok:
			return true
		default:
			return a.Before(b)
		}
	})
	return history.NewFrame(Columns, rows)
}

// Summarize は倍率の帯ごとの件数・平均リターン・勝率・投下額。
//
// 増額の帯（1.0 超）が通常の帯より高いリターンなら、倍率の付け方は効いている。
func Summarize(evaluation history.Frame) history.Frame {
	type agg struct {
		count int64
		sum   float64
		wins  int64
		due   float64
	}
	byBucket := map[string]*agg{}
	for _, row := range evaluation.Rows {
		ret := floatOf(row["ret_bp"])
		if ret == nil {
			continue
		}
		bucket, _ := row["bucket"].(string)
		a := byBucket[bucket]
		if a == nil {
			a = &agg{}
			byBucket[bucket] = a
		}
		a.count++
		a.sum += *ret
		if *ret > 0 {
			a.wins++
		}
		if d := floatOf(row["due"]); d != nil {
			a.due += *d
		}
	}

	rows := []map[string]any{}
	for _, bucket := range Buckets {
		a := byBucket[bucket]
		if a == nil {
			continue
		}
		rows = append(rows, map[string]any{
			"bucket":     bucket,
			"count":      a.count,
			"avg_ret_bp": a.sum / float64(a.count),
			"win_rate":   float64(a.wins) / float64(a.count),
			"due":        a.due,
		})
	}
	return history.NewFrame(SummaryColumns, rows)
}

// Scored は実績の出た行だけ。履歴が 1 件も無いと列自体が無い空の表が来るので、
// 列の有無に依存しない形で絞る。
func Scored(evaluation history.Frame) history.Frame {
	return evaluation.Filter(func(row map[string]any) bool { return floatOf(row["ret_bp"]) != nil })
}

func floatOf(value any) *float64 {
	switch v := value.(type) {
	case nil:
		return nil
	case float64:
		return &v
	case float32:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	case int:
		f := float64(v)
		return &f
	default:
		if f, ok := history.ToFloat(value).(float64); ok {
			return &f
		}
		return nil
	}
}

// dayOf は履歴の日付列（time.Time か文字列）を "YYYY-MM-DD" にする。
func dayOf(value any) string {
	switch v := value.(type) {
	case time.Time:
		return v.UTC().Format("2006-01-02")
	case string:
		if len(v) >= 10 {
			return v[:10]
		}
	}
	return ""
}

func mustDay(day string) time.Time {
	t, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}
