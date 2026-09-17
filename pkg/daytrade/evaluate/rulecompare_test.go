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

// 後の回で建てた行は比べから外す。1 回目が N 件に届かなかった朝に、後の回の選定だけが
// LightGBM 側に足されると片側の件数が増えて比べにならない（2026-09-18 のレビュー）。
func TestCompareRuleExcludesLaterRuns(t *testing.T) {
	rows := ranking()
	rows[0].Score, rows[0].RuleRank, rows[0].RulePicked = f(0.6), 1, true
	rows[1].Score, rows[1].RuleRank = f(0.5), 2
	// 後の回で建てた行（1 回目の選定ではない）
	rows[2].Score, rows[2].RuleRank, rows[2].Picked, rows[2].LaterRun = f(0.4), 3, true, true
	rows[2].Quantity, rows[2].Amount = f(100), f(99000)
	result := evaluate.Evaluate(rows, "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	c, ok := evaluate.CompareRule(result)
	if !ok {
		t.Fatal("LightGBM の日と見なさない")
	}
	if c.LGBM.Count != 1 || c.LGBM.Symbols[0] != "1000" {
		t.Errorf("後の回の選定が比べに入った: %+v", c.LGBM)
	}
	if c.LaterRuns != 1 {
		t.Errorf("外した件数 = %d, want 1", c.LaterRuns)
	}
}

// 既存規則なら建てない日（米国小幅高）は rule_off が立ち、gap_vol 側 0 件が正しい姿。
func TestCompareRuleRuleOff(t *testing.T) {
	rows := ranking()
	rows[0].Score, rows[0].RuleRank, rows[0].RuleOff = f(0.6), 1, true
	rows[1].Score, rows[1].RuleRank, rows[1].RuleOff = f(0.5), 2, true
	rows[2].Score, rows[2].RuleRank, rows[2].RuleOff = f(0.4), 3, true
	result := evaluate.Evaluate(rows, "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	c, ok := evaluate.CompareRule(result)
	if !ok || !c.RuleOff {
		t.Fatalf("rule_off = %v（ok=%v）", c.RuleOff, ok)
	}
	if c.Rule.Count != 0 || c.Rule.EvenPnL != 0 {
		t.Errorf("建てない日なのに gap_vol 側に成績が付いた: %+v", c.Rule)
	}
}

// 円で比べるのは even_pnl（等金額）。picked の hypo_pnl は按分の株数なので物差しが違う。
func TestCompareRuleEvenPnLUsesSameSizing(t *testing.T) {
	rows := ranking()
	// 実際は 100 株建てたが、1 注文 100,000 円の等金額なら 100 株 × 始値 940 = 94,000 円ぶん
	rows[0].Score, rows[0].RuleRank = f(0.6), 1
	rows[0].Quantity, rows[0].Amount = f(200), f(190000)
	rows[1].Score, rows[1].RuleRank, rows[1].RulePicked = f(0.5), 2, true
	result := evaluate.Evaluate(rows, "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	for _, row := range result.Rows {
		if row["symbol"] != "1000" {
			continue
		}
		if row["even_quantity"] == row["hypo_quantity"] {
			t.Errorf("按分の株数と等金額の株数が同じ: %v", row["even_quantity"])
		}
	}
	c, _ := evaluate.CompareRule(result)
	if c.LGBM.EvenPnL == c.LGBM.HypoPnL {
		t.Errorf("even_pnl と hypo_pnl が同じ（物差しを揃えていない）: %v", c.LGBM.EvenPnL)
	}
}
