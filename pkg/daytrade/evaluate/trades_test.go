package evaluate_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// evalColumns は履歴に実際に載る列（評価の列 ＋ 台帳が足す day / recorded_at）。
func evalColumns() []history.Column {
	return append([]history.Column{
		{Name: "day", Type: history.TypeDate},
		{Name: "recorded_at", Type: history.TypeTimestamp},
	}, evaluate.EvaluationSchema...)
}

type evalRow struct {
	day      string
	at       time.Time
	side     string
	rank     int64
	code     string
	name     string
	group    string
	picked   bool
	netBP    float64
	pnl      float64
	open     float64
	close    float64
	quantity float64
	// 実発注の約定単価（0 なら建てていない）。
	actualEntry, actualExit, actualPnL float64
	limitUpClose                       bool
}

func rowOf(r evalRow) map[string]any {
	day, _ := time.Parse("2006-01-02", r.day)
	at := r.at
	if at.IsZero() {
		at = day.Add(20 * time.Hour)
	}
	row := map[string]any{
		"day": day, "recorded_at": at,
		"ranking_source": evaluate.SourceArchiveOpen, "ranking_run_id": "run",
		"side": r.side, "rank": r.rank, "symbol": r.code[:4], "code": r.code, "name": r.name,
		"rank_group": r.group, "prev_close": r.open, "price": r.open, "gap": -0.03,
		"vol20": nil, "picked": r.picked, "quantity": r.quantity, "amount": r.quantity * r.open,
		"n": int64(3), "budget": 670000.0,
		"open": r.open, "high": r.open, "low": r.open, "close": r.close,
		"gap_open": -0.03, "ret_oc": r.close/r.open - 1,
		"gross_bp": r.netBP + 5, "cost_bp": 5.0, "net_bp": r.netBP,
		"hypo_quantity": r.quantity, "hypo_pnl": r.pnl,
		"limit_up_close": r.limitUpClose, "limit_down_close": false,
		"ul_flag": nil, "ll_flag": nil,
		"traded": false, "filled_quantity": nil,
		"actual_entry": nil, "actual_exit": nil, "actual_pnl": nil,
	}
	if r.actualEntry > 0 {
		row["traded"] = true
		row["filled_quantity"] = r.quantity
		row["actual_entry"] = r.actualEntry
		row["actual_exit"] = r.actualExit
		row["actual_pnl"] = r.actualPnL
	}
	return row
}

func frameOf(rows ...evalRow) history.Frame {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowOf(r))
	}
	return history.NewFrame(evalColumns(), out)
}

func find(t *testing.T, frame history.Frame, code string) map[string]any {
	t.Helper()
	for _, row := range frame.Rows {
		if row["code"] == code {
			return row
		}
	}
	t.Fatalf("%s が明細にない", code)
	return nil
}

// 明細は建てた銘柄だけを、名称・株数・建値・手仕舞い値つきで並べる。
func TestTradesListsPickedWithPrices(t *testing.T) {
	frame := frameOf(
		evalRow{day: "2026-09-03", side: "BUY", rank: 1, code: "69960", name: "ニチコン",
			group: "picked", picked: true, netBP: -21.7, pnl: -1085, open: 2501, close: 2497, quantity: 200},
		evalRow{day: "2026-09-03", side: "BUY", rank: 2, code: "96820", name: "ＤＴＳ",
			group: "picked", picked: true, netBP: 121.1, pnl: 14708, open: 1104, close: 1118, quantity: 1100},
		// 建てていない候補は明細に出さない（次点として day_all_bp には効く）
		evalRow{day: "2026-09-03", side: "BUY", rank: 4, code: "12340", name: "次点",
			group: "next", netBP: 50, pnl: 0, open: 1000, close: 1005, quantity: 100},
	)
	trades := evaluate.Trades(frame)
	if trades.Height() != 2 {
		t.Fatalf("明細 %d 行, want 2（建てた銘柄だけ）", trades.Height())
	}
	row := find(t, trades, "69960")
	if row["name"] != "ニチコン" || row["quantity"] != 200.0 {
		t.Errorf("名称・株数が入っていない: %+v", row)
	}
	if row["entry"] != 2501.0 || row["exit"] != 2497.0 {
		t.Errorf("建値・手仕舞い値 = %v / %v, want 2501 / 2497", row["entry"], row["exit"])
	}
	if row["priced"] != evaluate.PricedBar {
		t.Errorf("価格の出どころ = %v, want %s（建てていないので日足）", row["priced"], evaluate.PricedBar)
	}
	if row["net_bp"] != -21.7 || row["pnl"] != -1085.0 {
		t.Errorf("bp・損益が入っていない: %+v", row)
	}
	// 候補全体の平均（-21.7 + 121.1 + 50）/3 > 0 なので、負けた 69960 は「選定負け」
	if row["cause"] != evaluate.CauseSelectionLoss {
		t.Errorf("主因 = %v, want %s", row["cause"], evaluate.CauseSelectionLoss)
	}
	if find(t, trades, "96820")["cause"] != evaluate.CauseMarketWin {
		t.Errorf("候補全体も勝っている日の勝ちは地合い勝ち")
	}
}

// 実際に建てた取引は、日足ではなく台帳の約定単価を使う。
func TestTradesPrefersActualFills(t *testing.T) {
	frame := frameOf(evalRow{
		day: "2026-09-03", side: "BUY", rank: 1, code: "72030", name: "トヨタ",
		group: "picked", picked: true, netBP: 40, pnl: 8000, open: 2500, close: 2520, quantity: 400,
		actualEntry: 2505, actualExit: 2518, actualPnL: 5200,
	})
	row := find(t, evaluate.Trades(frame), "72030")
	if row["entry"] != 2505.0 || row["exit"] != 2518.0 {
		t.Errorf("約定単価を使っていない: %v / %v", row["entry"], row["exit"])
	}
	if row["priced"] != evaluate.PricedActual {
		t.Errorf("価格の出どころ = %v, want %s", row["priced"], evaluate.PricedActual)
	}
	if row["pnl"] != 5200.0 {
		t.Errorf("損益 = %v, want 5200（実現損益）", row["pnl"])
	}
	if row["bar_open"] != 2500.0 || row["bar_close"] != 2520.0 {
		t.Errorf("日足の値も残すこと（執行の乖離を出すため）: %+v", row)
	}
}

// 引けストップ高の売建は「手仕舞えていない恐れ」を注意として残す。
func TestTradesFlagsPinnedClose(t *testing.T) {
	frame := frameOf(evalRow{
		day: "2026-09-03", side: "SELL", rank: 1, code: "30380", name: "神戸物産",
		group: "picked", picked: true, netBP: -200, pnl: -20000, open: 3000, close: 3300,
		quantity: 200, limitUpClose: true,
	})
	row := find(t, evaluate.Trades(frame), "30380")
	if row["note"] == "" {
		t.Error("引けストップ高の売建に注意が付いていない")
	}
}

// 同じ日に何度も evaluate していれば最後の 1 回だけを使う（二重計上しない）。
func TestTradesUsesLatestRunPerDay(t *testing.T) {
	early := time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC)
	late := time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)
	frame := frameOf(
		evalRow{day: "2026-09-03", at: early, side: "BUY", rank: 1, code: "69960", name: "旧",
			group: "picked", picked: true, netBP: 10, pnl: 100, open: 100, close: 101, quantity: 100},
		evalRow{day: "2026-09-03", at: late, side: "BUY", rank: 1, code: "69960", name: "新",
			group: "picked", picked: true, netBP: 20, pnl: 200, open: 100, close: 102, quantity: 100},
	)
	trades := evaluate.Trades(frame)
	if trades.Height() != 1 {
		t.Fatalf("明細 %d 行, want 1（最後の evaluate だけ）", trades.Height())
	}
	if trades.Rows[0]["name"] != "新" {
		t.Errorf("古い評価を使っている: %+v", trades.Rows[0])
	}
}

// 合計はペイオフ・期待値・利益の集中・勝敗 × 主因を出す。
func TestTradeTotalsRiskMetrics(t *testing.T) {
	frame := frameOf(
		// 勝ち 2 件（+300 / +100）、負け 2 件（−100 / −100）
		evalRow{day: "2026-09-03", side: "BUY", rank: 1, code: "10000", name: "大勝ち",
			group: "picked", picked: true, netBP: 300, pnl: 30000, open: 100, close: 103, quantity: 100},
		evalRow{day: "2026-09-03", side: "BUY", rank: 2, code: "20000", name: "小勝ち",
			group: "picked", picked: true, netBP: 100, pnl: 10000, open: 100, close: 101, quantity: 100},
		evalRow{day: "2026-09-04", side: "BUY", rank: 1, code: "30000", name: "負け1",
			group: "picked", picked: true, netBP: -100, pnl: -10000, open: 100, close: 99, quantity: 100},
		evalRow{day: "2026-09-04", side: "BUY", rank: 2, code: "40000", name: "負け2",
			group: "picked", picked: true, netBP: -100, pnl: -10000, open: 100, close: 99, quantity: 100},
	)
	totals := evaluate.TradeTotals(evaluate.Trades(frame))
	if totals.Height() != 1 {
		t.Fatalf("合計 %d 行, want 1（買いだけ）", totals.Height())
	}
	row := totals.Rows[0]
	if row["trades"] != int64(4) || row["wins"] != int64(2) {
		t.Errorf("件数 = %+v", row)
	}
	if row["avg_win_bp"] != 200.0 || row["avg_loss_bp"] != -100.0 {
		t.Errorf("平均利益・平均損失 = %v / %v, want 200 / -100", row["avg_win_bp"], row["avg_loss_bp"])
	}
	if payoff, _ := row["payoff"].(float64); payoff != 2.0 {
		t.Errorf("ペイオフ = %v, want 2.0", row["payoff"])
	}
	if row["expectancy_bp"] != 50.0 {
		t.Errorf("期待値 = %v, want 50（(300+100-100-100)/4）", row["expectancy_bp"])
	}
	// 勝ち益の合計 400 のうち上位 1 件が 300 = 75%
	if share, _ := row["top1_share"].(float64); share != 0.75 {
		t.Errorf("上位 1 件の集中 = %v, want 0.75", row["top1_share"])
	}
	// 2 日とも候補全体＝建てた銘柄なので、勝ち負けはどちらも地合い
	if row["market_win"] != int64(2) || row["market_loss"] != int64(2) {
		t.Errorf("勝敗 × 主因 = %+v", row)
	}
	if row["pnl"] != 20000.0 {
		t.Errorf("損益 = %v, want 20000", row["pnl"])
	}
	if row["traded"] != int64(0) {
		t.Errorf("実発注 = %v, want 0（すべて想定）", row["traded"])
	}
}

// 執行の乖離は、実際の約定と日足（＝検証の前提）の差。買いは不利なら負。
func TestTradeTotalsSlippage(t *testing.T) {
	frame := frameOf(evalRow{
		day: "2026-09-03", side: "BUY", rank: 1, code: "10000", name: "滑り",
		group: "picked", picked: true, netBP: 50, pnl: 1000, open: 1000, close: 1010, quantity: 100,
		// 想定は 1000 → 1010（+100 bp）。実際は 1002 で建て 1008 で手仕舞い（+59.9 bp）
		actualEntry: 1002, actualExit: 1008, actualPnL: 600,
	})
	totals := evaluate.TradeTotals(evaluate.Trades(frame))
	slippage, ok := totals.Rows[0]["slippage_bp"].(float64)
	if !ok {
		t.Fatalf("執行の乖離が出ていない: %+v", totals.Rows[0])
	}
	if slippage > -30 || slippage < -50 {
		t.Errorf("執行の乖離 = %.1f bp, want ≈ -40（不利なので負）", slippage)
	}
	if totals.Rows[0]["traded"] != int64(1) {
		t.Errorf("実発注の件数が数えられていない: %+v", totals.Rows[0])
	}
}

func TestTradesEmpty(t *testing.T) {
	trades := evaluate.Trades(history.NewFrame(evalColumns(), nil))
	if trades.Height() != 0 {
		t.Errorf("空の履歴から %d 行", trades.Height())
	}
	if evaluate.TradeTotals(trades).Height() != 0 {
		t.Error("空の明細から合計が出た")
	}
}
