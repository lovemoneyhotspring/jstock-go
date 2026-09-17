package evaluate

import "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"

// PickSet は 1 つの選び方で建てていたら（件数・平均 net bp・想定損益の合計）。
type PickSet struct {
	// Count は選んだ件数、Priced はそのうち日足が当たった件数（平均と損益はこの分だけ）。
	Count  int
	Priced int
	// AvgNetBP は net bp の単純平均（Priced が 0 なら nil）。
	AvgNetBP *float64
	// HypoPnL は「建てていたら」の円損益の合計（hypo_pnl）。
	HypoPnL float64
	// Symbols は選んだ銘柄（順位順）。
	Symbols []string
}

// RuleComparison は LightGBM で並べた日の、ロングの選定の比べ。
type RuleComparison struct {
	// LGBM は実際の選定（picked）、Rule は既存規則（gap_vol）なら選んでいた銘柄（rule_picked）。
	LGBM PickSet
	Rule PickSet
	// Overlap は両方で選んだ件数。
	Overlap int
	// Skipped は危険信号で見送った日（両方とも「建てていたら」）。
	Skipped bool
}

// CompareRule は評価結果（Evaluate の出力）から、ロングの LightGBM と既存規則の成績を並べる。
// LightGBM で並べていない日（score の無い日）は ok = false。
func CompareRule(result history.Frame) (c RuleComparison, ok bool) {
	add := func(set *PickSet, row map[string]any) {
		set.Count++
		set.Symbols = append(set.Symbols, str(row["symbol"]))
		net := floatPtrOf(row["net_bp"])
		if net == nil {
			return
		}
		sum := 0.0
		if set.AvgNetBP != nil {
			sum = *set.AvgNetBP * float64(set.Priced)
		}
		set.Priced++
		avg := (sum + *net) / float64(set.Priced)
		set.AvgNetBP = &avg
		set.HypoPnL += floatOf(row["hypo_pnl"])
	}
	for _, row := range result.Rows {
		if str(row["side"]) != "BUY" {
			continue
		}
		if row["score"] != nil {
			ok = true
		}
		if boolOf(row["skipped"]) {
			c.Skipped = true
		}
		picked, rule := boolOf(row["picked"]), boolOf(row["rule_picked"])
		if picked {
			add(&c.LGBM, row)
		}
		if rule {
			add(&c.Rule, row)
		}
		if picked && rule {
			c.Overlap++
		}
	}
	return c, ok
}

// Fields はログ（daytrade.evaluate）に載せる形。
func (c RuleComparison) Fields() map[string]any {
	avg := func(v *float64) any {
		if v == nil {
			return nil
		}
		return *v
	}
	return map[string]any{
		"lgbm_picks": c.LGBM.Symbols, "lgbm_avg_net_bp": avg(c.LGBM.AvgNetBP), "lgbm_hypo_pnl": c.LGBM.HypoPnL,
		"rule_picks": c.Rule.Symbols, "rule_avg_net_bp": avg(c.Rule.AvgNetBP), "rule_hypo_pnl": c.Rule.HypoPnL,
		"overlap": c.Overlap, "skipped": c.Skipped,
	}
}
