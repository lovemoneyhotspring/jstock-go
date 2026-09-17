package evaluate_test

import (
	"math"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
)

// LightGBM で並べた日は、実際の選定と既存規則の選定を並べて比べる。
func TestCompareRule(t *testing.T) {
	rows := ranking()
	// LightGBM は 1000 を選び、既存規則なら 2000 を選んでいた
	rows[0].Score, rows[0].RuleRank = f(0.6), 2
	rows[1].Score, rows[1].RuleRank, rows[1].RulePicked = f(0.5), 1, true
	rows[2].Score, rows[2].RuleRank = f(0.4), 3
	result := evaluate.Evaluate(rows, "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	for _, row := range result.Rows {
		if row["symbol"] == "2000" && (row["rule_picked"] != true || row["rule_rank"] != int64(1) || row["score"] != 0.5) {
			t.Errorf("既存規則の列 = %v %v %v", row["rule_picked"], row["rule_rank"], row["score"])
		}
	}
	c, ok := evaluate.CompareRule(result)
	if !ok {
		t.Fatal("LightGBM の日と見なさない")
	}
	if c.LGBM.Count != 1 || c.LGBM.Symbols[0] != "1000" || c.Rule.Count != 1 || c.Rule.Symbols[0] != "2000" || c.Overlap != 0 {
		t.Errorf("選定 = %+v / %+v / 重なり %d", c.LGBM, c.Rule, c.Overlap)
	}
	if c.LGBM.AvgNetBP == nil || c.Rule.AvgNetBP == nil || !(*c.LGBM.AvgNetBP > 0) || !(*c.Rule.AvgNetBP < 0) {
		t.Errorf("平均 net bp = %v / %v", c.LGBM.AvgNetBP, c.Rule.AvgNetBP)
	}
	if math.Abs(c.Rule.HypoPnL) == 0 {
		t.Error("既存規則の想定損益が 0")
	}
}

// LightGBM で並べていない日（score 無し）・記録の無い古い順位表は比べない。
func TestCompareRuleWithoutScore(t *testing.T) {
	result := evaluate.Evaluate(ranking(), "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	if _, ok := evaluate.CompareRule(result); ok {
		t.Error("score の無い日を比べた")
	}
	for _, row := range result.Rows {
		if row["rule_rank"] != nil || row["rule_picked"] != nil {
			t.Errorf("古い順位表で rule_* が埋まった: %v %v", row["rule_rank"], row["rule_picked"])
		}
	}
}
