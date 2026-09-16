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

// 引けの手仕舞いが 60 株だけ約定し、残り 40 株を翌寄りの持ち越し返済で手仕舞った。
// 手仕舞いが 2 本でも 100 株ぶんの損益と加重平均の単価になる（最後の 1 本で上書きしない）。
func TestEvaluateJoinsLedgerWithMultipleExits(t *testing.T) {
	buy := dec(1000)
	closeSell := dec(1050)
	carrySell := dec(1100)
	orders := []dtledger.Order{
		{Symbol: "1000", Side: "BUY", Trade: "MARGIN_OPEN", Status: "FILLED",
			Quantity: dec(100), FilledQuantity: dec(100), AvgFillPrice: &buy},
		{Symbol: "1000", Side: "SELL", Trade: "MARGIN_CLOSE", Status: "PARTIALLY_FILLED",
			Quantity: dec(100), FilledQuantity: dec(60), AvgFillPrice: &closeSell},
		{Symbol: "1000", Side: "SELL", Trade: "MARGIN_CLOSE", Status: "FILLED",
			Quantity: dec(40), FilledQuantity: dec(40), AvgFillPrice: &carrySell},
	}
	result := evaluate.Evaluate(ranking(), "", bars(), baseConfig(), orders, evaluate.SourceQuotes)
	for _, row := range result.Rows {
		if row["symbol"] != "1000" {
			continue
		}
		// 60 × 50 + 40 × 100 = 7,000 円（最後の 1 本だけなら 4,000 円）
		if pnl, _ := row["actual_pnl"].(float64); pnl != 7000 {
			t.Errorf("実現損益 = %v, want 7000", row["actual_pnl"])
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

func TestEvaluateCarriesSkippedAndOverBudget(t *testing.T) {
	rows := ranking()
	for i := range rows {
		rows[i].Skipped = true
	}
	rows[1].OverBudget = true
	frame := evaluate.Evaluate(rows, "r1", bars(), baseConfig(), nil, evaluate.SourceQuotes)
	for _, row := range frame.Rows {
		if row["skipped"] != true {
			t.Errorf("%v: skipped が立っていない", row["symbol"])
		}
		if want := row["symbol"] == "2000"; row["over_budget"] != want {
			t.Errorf("%v: over_budget = %v, want %v", row["symbol"], row["over_budget"], want)
		}
	}
}

// 見送りの日（危険信号で建てなかった日の「建てていたら」）は、通常日の合計に混ぜない。
func TestReviewSeparatesSkippedDays(t *testing.T) {
	traded := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	skipped := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	row := func(day time.Time, skip bool, group string, bp float64) map[string]any {
		return map[string]any{"day": day, "recorded_at": day.Add(12 * time.Hour), "side": "BUY",
			"rank_group": group, "ranking_source": evaluate.SourceQuotes, "skipped": skip,
			"net_bp": bp, "hypo_pnl": bp * 10, "actual_pnl": nil, "traded": false}
	}
	frame := history.NewFrame(evaluate.EvaluationSchema, []map[string]any{
		row(traded, false, "picked", -50), row(traded, false, "next", 10),
		row(skipped, true, "picked", 300), row(skipped, true, "next", 100),
	})
	table := evaluate.Review(frame)
	if table.Height() != 2 || table.Rows[0]["skipped"] != false || table.Rows[1]["skipped"] != true {
		t.Fatalf("日別の skipped が違う: %+v", table.Rows)
	}
	totals := evaluate.ReviewTotals(table)
	if totals.Height() != 2 {
		t.Fatalf("合計 %d 行, want 2（通常日と見送りの日）", totals.Height())
	}
	if totals.Rows[0]["skipped"] != false {
		t.Errorf("通常日の合計が先に並んでいない: %+v", totals.Rows)
	}
	for _, tot := range totals.Rows {
		want := -50.0
		if tot["skipped"] == true {
			want = 300
		}
		if tot["days"].(int64) != 1 || tot["picked_bp"].(float64) != want {
			t.Errorf("skipped=%v: days %v picked_bp %v, want 1 / %v", tot["skipped"], tot["days"], tot["picked_bp"], want)
		}
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

// rankingRunFrame は順位表の回を 2 つ持つ最小の表（recorded_at と picked だけ見る）。
func rankingRunFrame(rows []map[string]any) history.Frame {
	return history.NewFrame([]history.Column{
		{Name: "recorded_at", Type: history.TypeTimestamp},
		{Name: "symbol", Type: history.TypeString},
		{Name: "picked", Type: history.TypeBool},
	}, rows)
}

// rankingSkipFrame は side と skipped も持つ順位表（見送りの回を混ぜた朝を作るため）。
func rankingSkipFrame(rows []map[string]any) history.Frame {
	return history.NewFrame([]history.Column{
		{Name: "recorded_at", Type: history.TypeTimestamp},
		{Name: "symbol", Type: history.TypeString},
		{Name: "side", Type: history.TypeString},
		{Name: "picked", Type: history.TypeBool},
		{Name: "skipped", Type: history.TypeBool},
	}, rows)
}

// TestPickRankingRunIgnoresSkippedRun は、危険信号で見送った回を「建てた回」として採らないこと。
// 見送りの回にも「建てていたら」の picked が立つので（appendSkippedRanking）、skipped を見ないと
// 1 株も建てていない仮想の回を評価してしまう。2026-09-15 がこの形だった
// （9:01〜9:10 の 4 回が見送りで picked 3〜5、9:13 だけが本物）。
func TestPickRankingRunIgnoresSkippedRun(t *testing.T) {
	skipped := time.Date(2026, 9, 15, 0, 1, 4, 0, time.UTC)
	real := time.Date(2026, 9, 15, 0, 13, 4, 0, time.UTC)
	frame := rankingSkipFrame([]map[string]any{
		// 見送った回。建てていたら選んでいた銘柄に picked が立つ
		{"recorded_at": skipped, "symbol": "6875", "side": "BUY", "picked": true, "skipped": true},
		{"recorded_at": skipped, "symbol": "5310", "side": "BUY", "picked": false, "skipped": true},
		// 実際に建てた回
		{"recorded_at": real, "symbol": "5310", "side": "BUY", "picked": true, "skipped": false},
		{"recorded_at": real, "symbol": "6875", "side": "BUY", "picked": false, "skipped": false},
	})
	got, info := evaluate.PickRankingRun(frame)
	if !info.At.Equal(real) || info.Skipped || info.Fallback {
		t.Fatalf("見送りの回を採っている: %+v", info)
	}
	if info.Picked != 1 {
		t.Errorf("picked = %d, want 1（仮想の 6875 を数えない）", info.Picked)
	}
	for _, row := range got.Rows {
		if at := row["recorded_at"].(time.Time); !at.Equal(real) {
			t.Errorf("見送りの回の行が残っている: %v %v", at, row["symbol"])
		}
	}
}

// TestPickRankingRunUsesSkippedRunOnlyWhenAllSkipped は、本当の見送り日（全部の回が見送り）は
// 従来どおり「建てていたら」の順位表を評価すること。
func TestPickRankingRunUsesSkippedRunOnlyWhenAllSkipped(t *testing.T) {
	first := time.Date(2026, 9, 14, 0, 1, 4, 0, time.UTC)
	later := time.Date(2026, 9, 14, 0, 4, 4, 0, time.UTC)
	frame := rankingSkipFrame([]map[string]any{
		{"recorded_at": first, "symbol": "6875", "side": "BUY", "picked": true, "skipped": true},
		{"recorded_at": later, "symbol": "6875", "side": "BUY", "picked": true, "skipped": true},
	})
	_, info := evaluate.PickRankingRun(frame)
	if !info.At.Equal(first) || !info.Skipped || info.Fallback {
		t.Fatalf("見送り日の回の選び方: %+v", info)
	}
	if info.Picked != 1 {
		t.Errorf("picked = %d, want 1（同じ銘柄を 2 回数えない）", info.Picked)
	}
}

// TestPickRankingRunKeepsLatestPick は、同じ銘柄が複数の回で picked になった朝に
// **最後の回の行だけ**を残すこと。1 回目の注文が拒否・未約定だと候補に残り、次の回が
// 別の株数で建て直す。両方残すと台帳の同じ約定を 2 行に結び、実現損益が二重になる。
func TestPickRankingRunKeepsLatestPick(t *testing.T) {
	first := time.Date(2026, 9, 15, 0, 1, 4, 0, time.UTC)
	later := time.Date(2026, 9, 15, 0, 13, 4, 0, time.UTC)
	frame := rankingSkipFrame([]map[string]any{
		{"recorded_at": first, "symbol": "5020", "side": "BUY", "picked": true, "skipped": false},
		{"recorded_at": later, "symbol": "5020", "side": "BUY", "picked": true, "skipped": false},
	})
	got, info := evaluate.PickRankingRun(frame)
	if info.Picked != 1 {
		t.Fatalf("picked = %d, want 1（二重に数えない）: %+v", info.Picked, info)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("残った行 = %d, want 1", len(got.Rows))
	}
	if at := got.Rows[0]["recorded_at"].(time.Time); !at.Equal(later) {
		t.Errorf("残った行の回 = %v, want %v（建てたのは後の回）", at, later)
	}
}

// TestPickRankingRunPrefersRunWithPicks は、建て終わった後の回（picked 0）ではなく
// picks のある最初の回を評価に使うこと。最後の回を採ると選定が丸ごと抜ける（2026-09-16）。
func TestPickRankingRunPrefersRunWithPicks(t *testing.T) {
	first := time.Date(2026, 9, 16, 0, 1, 6, 0, time.UTC)
	later := time.Date(2026, 9, 16, 0, 13, 3, 0, time.UTC)
	frame := rankingRunFrame([]map[string]any{
		{"recorded_at": first, "symbol": "8136", "picked": true},
		{"recorded_at": first, "symbol": "3445", "picked": false},
		// 建て終わった後の回。建玉は候補から外れるので picks が 1 件も無い
		{"recorded_at": later, "symbol": "3445", "picked": false},
	})
	got, info := evaluate.PickRankingRun(frame)
	if info.Runs != 2 || info.Picked != 1 || info.Fallback {
		t.Fatalf("回の選び方: %+v", info)
	}
	if !info.At.Equal(first) {
		t.Errorf("採った回 = %v, want %v", info.At, first)
	}
	if got.Height() != 2 || got.Rows[0]["symbol"] != "8136" {
		t.Errorf("picks のある最初の回を採っていない: %+v", got.Rows)
	}
}

// TestPickRankingRunKeepsPicksFromLaterRuns は、1 回目で建てきれず次の回が**別の銘柄**で
// 残りを埋めた朝（締め切り・余力不足・発注失敗による部分約定）に、後の回の picked 行も
// 拾うこと。拾わないと、その約定は順位表のどの行にも一致せず Evaluate が黙って捨てる。
func TestPickRankingRunKeepsPicksFromLaterRuns(t *testing.T) {
	first := time.Date(2026, 9, 16, 0, 1, 6, 0, time.UTC)
	later := time.Date(2026, 9, 16, 0, 4, 3, 0, time.UTC)
	frame := rankingRunFrame([]map[string]any{
		{"recorded_at": first, "symbol": "8136", "picked": true},
		{"recorded_at": first, "symbol": "3445", "picked": false},
		// 2 回目は建て済みの 8136 を候補から外し、残りの枚数を 9984 で埋めた
		{"recorded_at": later, "symbol": "8136", "picked": false},
		{"recorded_at": later, "symbol": "9984", "picked": true},
	})
	got, info := evaluate.PickRankingRun(frame)
	if !info.At.Equal(first) {
		t.Fatalf("採った回 = %v, want %v", info.At, first)
	}
	if info.Extra != 1 || info.Picked != 2 {
		t.Fatalf("後の回の picked を拾えていない: %+v", info)
	}
	picked := map[string]bool{}
	for _, row := range got.Rows {
		if row["picked"] == true {
			picked[row["symbol"].(string)] = true
		}
	}
	if !picked["8136"] || !picked["9984"] {
		t.Errorf("picked の和集合になっていない: %+v", got.Rows)
	}
	// 後の回から拾うのは picked だけ（同じ銘柄の非 picked 行を二重に持たない）
	if got.Height() != 3 {
		t.Errorf("行数 %d, want 3（1 回目の 2 行 + 2 回目の 9984）", got.Height())
	}
}

// TestPickRankingRunDropsDuplicateWhenLaterRunPicks は、選んだ回に**候補として**載っていた
// 銘柄が後の回で picked になった朝に、同じ銘柄が 2 行残らないこと。2 行あると Evaluate が
// 台帳の同じ約定を両方に結び、その朝の実現損益と約定件数が倍になる（2026-09-16 のレビュー）。
func TestPickRankingRunDropsDuplicateWhenLaterRunPicks(t *testing.T) {
	first := time.Date(2026, 9, 16, 0, 1, 6, 0, time.UTC)
	later := time.Date(2026, 9, 16, 0, 4, 3, 0, time.UTC)
	frame := rankingRunFrame([]map[string]any{
		{"recorded_at": first, "symbol": "8136", "picked": true},
		// 1 回目は候補どまりで、2 回目に建てた。残すのは 2 回目の picked 行だけ
		{"recorded_at": first, "symbol": "9984", "picked": false},
		{"recorded_at": later, "symbol": "9984", "picked": true},
	})
	got, info := evaluate.PickRankingRun(frame)
	if !info.At.Equal(first) || info.Extra != 1 || info.Picked != 2 {
		t.Fatalf("回の選び方: %+v", info)
	}
	count := map[string]int{}
	for _, row := range got.Rows {
		count[row["symbol"].(string)]++
	}
	if count["9984"] != 1 {
		t.Errorf("9984 が %d 行ある（実現損益が二重に乗る）: %+v", count["9984"], got.Rows)
	}
	if got.Height() != 2 {
		t.Errorf("行数 %d, want 2（1 回目の 8136 + 2 回目の 9984）", got.Height())
	}
	for _, row := range got.Rows {
		if row["symbol"] == "9984" && row["picked"] != true {
			t.Errorf("残った 9984 が建てた回の行でない: %+v", row)
		}
	}
}

// TestPickRankingRunFallsBackWhenNoPicks は、建てなかった日（picks のある回が無い）は
// 最後の回で代用し、その印を付けること。
func TestPickRankingRunFallsBackWhenNoPicks(t *testing.T) {
	first := time.Date(2026, 9, 16, 0, 1, 6, 0, time.UTC)
	later := time.Date(2026, 9, 16, 0, 13, 3, 0, time.UTC)
	frame := rankingRunFrame([]map[string]any{
		{"recorded_at": first, "symbol": "8136", "picked": false},
		{"recorded_at": later, "symbol": "3445", "picked": false},
	})
	got, info := evaluate.PickRankingRun(frame)
	if !info.Fallback || info.Runs != 2 || info.Picked != 0 {
		t.Fatalf("見送り日の代用が効いていない: %+v", info)
	}
	if !info.At.Equal(later) || got.Height() != 1 || got.Rows[0]["symbol"] != "3445" {
		t.Errorf("最後の回を採っていない: %+v %+v", info, got.Rows)
	}
}

// TestPickRankingRunWithoutTimestamps は recorded_at が無い表をそのまま返すこと。
func TestPickRankingRunWithoutTimestamps(t *testing.T) {
	frame := rankingRunFrame([]map[string]any{{"symbol": "8136", "picked": true}})
	got, info := evaluate.PickRankingRun(frame)
	if info.Runs != 0 || got.Height() != 1 {
		t.Errorf("素通ししていない: %+v %+v", info, got.Rows)
	}
}
