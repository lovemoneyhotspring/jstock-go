package evaluate_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
)

func f(v float64) *float64 { return &v }

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Capital.Weighting = "equal"
	return cfg
}

// ranking は「N=1、次点あり」の順位表。
func ranking() []evaluate.RankingRow {
	return []evaluate.RankingRow{
		{Side: "BUY", Rank: 1, Symbol: "1000", Code: "10000", Name: "選んだ銘柄",
			PrevClose: 1000, Price: 950, Gap: -0.05, Picked: true,
			Quantity: f(100), Amount: f(95000), N: 1, Budget: 100000},
		{Side: "BUY", Rank: 2, Symbol: "2000", Code: "20000", Name: "次点",
			PrevClose: 1000, Price: 970, Gap: -0.03, N: 1, Budget: 100000},
		{Side: "BUY", Rank: 10, Symbol: "3000", Code: "30000", Name: "圏外",
			PrevClose: 1000, Price: 990, Gap: -0.01, N: 1, Budget: 100000},
	}
}

func bars() map[string]evaluate.Bar {
	return map[string]evaluate.Bar{
		// 950 で寄って 1000 で引ける → +5.26%
		"10000": {Code: "10000", Open: 950, High: 1000, Low: 940, Close: 1000},
		"20000": {Code: "20000", Open: 970, High: 980, Low: 960, Close: 960},
		// 30000 は日足なし（結果の列が null のまま残る）
	}
}

func TestEvaluateAssignsRankGroups(t *testing.T) {
	result := evaluate.Evaluate(ranking(), "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	if result.Height() != 3 {
		t.Fatalf("行数 = %d", result.Height())
	}
	groups := map[string]string{}
	for _, row := range result.Rows {
		groups[row["symbol"].(string)] = row["rank_group"].(string)
	}
	// N=1 なので rank 1 が picked、rank 2〜6 が next、それ以降が rest
	want := map[string]string{"1000": "picked", "2000": "next", "3000": "rest"}
	for symbol, g := range want {
		if groups[symbol] != g {
			t.Errorf("%s の群 = %s, want %s", symbol, groups[symbol], g)
		}
	}
}

func TestEvaluateComputesReturns(t *testing.T) {
	result := evaluate.Evaluate(ranking(), "run-1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	byCode := map[string]map[string]any{}
	for _, row := range result.Rows {
		byCode[row["code"].(string)] = row
	}
	picked := byCode["10000"]
	// 950 → 1000 は +526 bp
	gross := picked["gross_bp"].(float64)
	if gross < 525 || gross > 527 {
		t.Errorf("gross_bp = %v, want ≈526", gross)
	}
	if picked["net_bp"].(float64) >= gross {
		t.Error("net が gross 以上（費用が引かれていない）")
	}
	if picked["hypo_quantity"].(float64) != 100 {
		t.Errorf("選んだ銘柄は記録の株数を使うはず: %v", picked["hypo_quantity"])
	}
	// 寄付ギャップ（実際）は 950/1000 − 1 = −5%
	if gapOpen := picked["gap_open"].(float64); gapOpen < -0.051 || gapOpen > -0.049 {
		t.Errorf("gap_open = %v", gapOpen)
	}
	if picked["ranking_source"] != evaluate.SourceQuotes || picked["ranking_run_id"] != "run-1" {
		t.Errorf("出所が残っていない: %v / %v", picked["ranking_source"], picked["ranking_run_id"])
	}
	// 日足の無い銘柄は結果が null（「なぜ評価できないか」を残す）
	if byCode["30000"]["net_bp"] != nil || byCode["30000"]["open"] != nil {
		t.Errorf("日足なしの行に値が入っている: %v", byCode["30000"])
	}
	// 次点は予算で買える株数を当てる（950 → 100 株ではなく 970 → 100 株）
	if byCode["20000"]["hypo_quantity"].(float64) != 100 {
		t.Errorf("次点の想定株数 = %v", byCode["20000"]["hypo_quantity"])
	}
}

func TestEvaluateShortSideFlipsSign(t *testing.T) {
	rows := []evaluate.RankingRow{{
		Side: "SELL", Rank: 1, Symbol: "2000", Code: "20000", Name: "売建",
		PrevClose: 1000, Price: 1080, Gap: 0.08, Picked: true,
		Quantity: f(100), Amount: f(108000), N: 1, Budget: 108000,
	}}
	// 1080 で売って 1000 で買い戻す → ショートは +740 bp
	b := map[string]evaluate.Bar{"20000": {Code: "20000", Open: 1080, Close: 1000}}
	cfg := baseConfig()
	cfg.Margin.Enabled = true
	result := evaluate.Evaluate(rows, "", b, cfg, nil, evaluate.SourceQuotes)
	row := result.Rows[0]
	if row["gross_bp"].(float64) <= 0 {
		t.Errorf("ショートの下落が損益プラスになっていない: %v", row["gross_bp"])
	}
	if row["hypo_pnl"].(float64) <= 0 {
		t.Errorf("ショートの円損益 = %v", row["hypo_pnl"])
	}
	// 費用は信用の見込み値（extra_cost_bp = 5）
	if cost := row["cost_bp"].(float64); cost != 5 {
		t.Errorf("cost_bp = %v, want 5", cost)
	}
}

func TestEvaluateJoinsLedger(t *testing.T) {
	buy := dec(1000)
	sell := dec(1050)
	orders := []dtledger.Order{
		{Symbol: "1000", Side: "BUY", Trade: "CASH", Status: "FILLED",
			Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &buy},
		{Symbol: "1000", Side: "SELL", Trade: "CASH", Status: "FILLED",
			Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &sell},
	}
	result := evaluate.Evaluate(ranking(), "", bars(), baseConfig(), orders, evaluate.SourceQuotes)
	for _, row := range result.Rows {
		if row["symbol"] != "1000" {
			if row["traded"].(bool) {
				t.Errorf("建てていない銘柄が traded になっている: %v", row["symbol"])
			}
			continue
		}
		if !row["traded"].(bool) {
			t.Error("台帳の約定が反映されていない")
		}
		if pnl := row["actual_pnl"].(float64); pnl != 5000 {
			t.Errorf("実現損益 = %v, want 5000", pnl)
		}
	}
}

func TestEvaluateSkipsBrokerVerify(t *testing.T) {
	// 実機検証（--broker-verify）の約定は成績ではないので、候補の評価に混ぜない。
	// env=prod で検証することがあるので、口座では切り分けられない
	buy := dec(1000)
	sell := dec(1050)
	orders := []dtledger.Order{
		{Symbol: "1000", Side: "BUY", Trade: "CASH", Status: "FILLED", Verify: true,
			Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &buy},
		{Symbol: "1000", Side: "SELL", Trade: "CASH", Status: "FILLED", Verify: true,
			Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &sell},
	}
	result := evaluate.Evaluate(ranking(), "", bars(), baseConfig(), orders, evaluate.SourceQuotes)
	for _, row := range result.Rows {
		if row["traded"].(bool) {
			t.Errorf("検証の約定が traded になっている: %v", row["symbol"])
		}
		if row["actual_pnl"] != nil {
			t.Errorf("検証の損益が実現損益に入った: %v", row["actual_pnl"])
		}
	}
}

func TestSummarize(t *testing.T) {
	result := evaluate.Evaluate(ranking(), "", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	summary := evaluate.Summarize(result)
	// 日足が無い rest は集計に入らない → picked と next の 2 行
	if summary.Height() != 2 {
		t.Fatalf("要約 %d 行, want 2: %+v", summary.Height(), summary.Rows)
	}
	if summary.Rows[0]["rank_group"] != "picked" {
		t.Errorf("群の並びが picked → next でない: %v", summary.Rows[0]["rank_group"])
	}
	if summary.Rows[0]["count"].(int64) != 1 {
		t.Errorf("件数 = %v", summary.Rows[0]["count"])
	}
	if summary.Rows[0]["win_rate"].(float64) != 1 {
		t.Errorf("勝率 = %v, want 1", summary.Rows[0]["win_rate"])
	}
	if evaluate.Summarize(history.Frame{}).Height() != 0 {
		t.Error("空の入力で行が出ている")
	}
}

func TestReviewAndTotals(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	rows := []map[string]any{
		{"day": day, "recorded_at": at, "side": "BUY", "rank_group": "picked",
			"ranking_source": evaluate.SourceQuotes, "net_bp": 100.0, "hypo_pnl": 5000.0,
			"actual_pnl": nil, "traded": false, "picked": true},
		{"day": day, "recorded_at": at, "side": "BUY", "rank_group": "next",
			"ranking_source": evaluate.SourceQuotes, "net_bp": 20.0, "hypo_pnl": 1000.0,
			"actual_pnl": nil, "traded": false, "picked": false},
	}
	frame := history.NewFrame(evaluate.EvaluationSchema, rows)
	table := evaluate.Review(frame)
	if table.Height() != 1 {
		t.Fatalf("日別 %d 行, want 1", table.Height())
	}
	row := table.Rows[0]
	if row["picked_bp"].(float64) != 100 || row["next_bp"].(float64) != 20 {
		t.Errorf("群ごとの平均が違う: %+v", row)
	}
	// all は候補全体の平均（100 と 20 の平均 = 60）
	if row["all_bp"].(float64) != 60 {
		t.Errorf("all_bp = %v, want 60", row["all_bp"])
	}
	if row["candidates"].(int64) != 2 || row["picked_n"].(int64) != 1 {
		t.Errorf("件数が違う: %+v", row)
	}

	totals := evaluate.ReviewTotals(table)
	if totals.Height() != 1 {
		t.Fatalf("合計 %d 行", totals.Height())
	}
	tot := totals.Rows[0]
	if tot["days"].(int64) != 1 {
		t.Errorf("日数 = %v", tot["days"])
	}
	// picked が prof かつ all を上回っている
	if tot["picked_win_days"].(float64) != 1 || tot["beat_all_days"].(float64) != 1 {
		t.Errorf("勝ち日・上回った日 = %v / %v", tot["picked_win_days"], tot["beat_all_days"])
	}
}

func TestLatestPerDayKeepsLastRun(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	early := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	frame := history.NewFrame(evaluate.EvaluationSchema, []map[string]any{
		{"day": day, "recorded_at": early, "symbol": "old"},
		{"day": day, "recorded_at": late, "symbol": "new"},
	})
	got := evaluate.LatestPerDay(frame)
	if got.Height() != 1 || got.Rows[0]["symbol"] != "new" {
		t.Errorf("最後の実行だけを残していない: %+v", got.Rows)
	}
}

func dec(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

func TestEvaluateAddsMiddayWhenMinuteBarsExist(t *testing.T) {
	rows := bars()
	// 950 で寄って 11:30 に 975（+2.63%）、引けは 1000
	bar := rows["10000"]
	bar.Midday = f(975)
	rows["10000"] = bar

	result := evaluate.Evaluate(ranking(), "run-1", rows, baseConfig(), nil, evaluate.SourceQuotes)
	byCode := map[string]map[string]any{}
	for _, row := range result.Rows {
		byCode[row["code"].(string)] = row
	}
	picked := byCode["10000"]
	if picked["midday"] != 975.0 {
		t.Errorf("midday = %v, want 975", picked["midday"])
	}
	gross := picked["midday_gross_bp"].(float64)
	if gross < 262 || gross > 264 {
		t.Errorf("midday_gross_bp = %v, want ≈ 263", gross)
	}
	// 費用は引けまで持ったときと同じ引き方
	if net, cost := picked["midday_net_bp"].(float64), picked["cost_bp"].(float64); net != gross-cost {
		t.Errorf("midday_net_bp = %v, want %v", net, gross-cost)
	}
	// 分足の無い銘柄は null のまま
	if v := byCode["20000"]["midday"]; v != nil {
		t.Errorf("分足の無い銘柄の midday = %v, want null", v)
	}
}
