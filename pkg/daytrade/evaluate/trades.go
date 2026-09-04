package evaluate

import (
	"slices"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
)

// 建てた 1 銘柄ずつの明細。
//
// review は日 × 脚の平均で「規則が効いているか」を見るが、それだけでは
// 「何を、いくらで建てて、いくらで手仕舞い、なぜ勝った（負けた）か」に答えられない。
// ここは 1 行 1 取引で、レポートがそのまま書ける形に並べる。
//
// 勝敗の理由を切り分けられるよう、その日その脚の**候補全体の平均**（day_all_bp）と
// **次点の平均**（day_next_bp）を各行に添える:
//
//   - net_bp < 0 かつ day_all_bp < 0 … 地合い。その日はどれを買っても負けた
//   - net_bp < 0 かつ day_all_bp > 0 … 選定。買わなかった銘柄は勝っていた
//   - net_bp > day_next_bp           … 次点より良い＝順位付けが効いた

// TradeSchema は 1 取引の表。
var TradeSchema = []history.Column{
	{Name: "day", Type: history.TypeDate},
	{Name: "side", Type: history.TypeString},
	{Name: "rank", Type: history.TypeInt64},
	{Name: "code", Type: history.TypeString},
	{Name: "name", Type: history.TypeString},
	{Name: "quantity", Type: history.TypeFloat64},
	// entry / exit は建値と手仕舞い値。actual_entry / actual_exit があればそれ、
	// 無ければ日足の始値・終値（priced が実際の約定か想定かを示す）。
	{Name: "entry", Type: history.TypeFloat64},
	{Name: "exit", Type: history.TypeFloat64},
	{Name: "priced", Type: history.TypeString},
	// bar_open / bar_close はその日の日足（検証が前提にしている値）。
	// entry / exit との差が執行の乖離＝バックテストとの差になる。
	{Name: "bar_open", Type: history.TypeFloat64},
	{Name: "bar_close", Type: history.TypeFloat64},
	{Name: "gap", Type: history.TypeFloat64},
	{Name: "gross_bp", Type: history.TypeFloat64},
	{Name: "cost_bp", Type: history.TypeFloat64},
	{Name: "net_bp", Type: history.TypeFloat64},
	{Name: "pnl", Type: history.TypeFloat64},
	{Name: "actual_pnl", Type: history.TypeFloat64},
	// day_next_bp / day_all_bp はその日その脚の次点・候補全体の平均 net bp（勝敗の切り分け）。
	{Name: "day_next_bp", Type: history.TypeFloat64},
	{Name: "day_all_bp", Type: history.TypeFloat64},
	// cause は勝敗の主因の当たり（地合い / 選定）。文章にするのは読み手の仕事。
	{Name: "cause", Type: history.TypeString},
	// note は張り付き等の注意（引けがストップ高・安なら手仕舞えていない恐れ）。
	{Name: "note", Type: history.TypeString},
}

// 価格の出どころ。
const (
	PricedActual = "actual" // 台帳の約定単価
	PricedBar    = "bar"    // 日足の始値・終値（建てていない、または約定単価が無い）
)

// 勝敗の主因。候補全体（day_all_bp）と同じ向きなら地合い、逆なら選定。
//
// 「選定勝ち」は候補全体が負けた日に勝った＝順位付けが価値を足した取引。
// 「選定負け」はその逆で、選ばなければ勝てた取引。地合いの勝ち負けは
// その日の相場に乗っただけなので、規則の良し悪しの証拠にはならない。
const (
	CauseMarketWin     = "地合い勝ち"
	CauseMarketLoss    = "地合い負け"
	CauseSelectionWin  = "選定勝ち"
	CauseSelectionLoss = "選定負け"
	CauseUnknown       = ""
)

// Trades は建てた銘柄（picked）を 1 行 1 取引で並べる。日付・脚・順位の順。
//
// 同じ日に何度も evaluate していれば最後の 1 回だけを使う（review と同じ）。
func Trades(evaluations history.Frame) history.Frame {
	scored := LatestPerDay(evaluations)
	if scored.Height() == 0 {
		return history.NewFrame(TradeSchema, nil)
	}

	// 先にその日その脚の平均（次点・候補全体）を作る
	type dayStats struct{ next, all []float64 }
	stats := map[string]*dayStats{}
	for _, row := range scored.Rows {
		netBP := floatPtrOf(row["net_bp"])
		if netBP == nil {
			continue
		}
		key := dayKey(row["day"]) + "|" + str(row["side"])
		s, ok := stats[key]
		if !ok {
			s = &dayStats{}
			stats[key] = s
		}
		s.all = append(s.all, *netBP)
		if str(row["rank_group"]) == "next" {
			s.next = append(s.next, *netBP)
		}
	}

	rows := make([]map[string]any, 0, len(scored.Rows))
	for _, row := range scored.Rows {
		if !boolOf(row["picked"]) {
			continue
		}
		netBP := floatPtrOf(row["net_bp"])
		key := dayKey(row["day"]) + "|" + str(row["side"])
		allBP := any(nil)
		nextBP := any(nil)
		if s, ok := stats[key]; ok {
			allBP, nextBP = mean(s.all), mean(s.next)
		}

		entry, exit, priced := pricesOf(row)
		rows = append(rows, map[string]any{
			"day":         row["day"],
			"side":        str(row["side"]),
			"rank":        intOf(row["rank"]),
			"code":        str(row["code"]),
			"name":        str(row["name"]),
			"quantity":    quantityOf(row),
			"entry":       entry,
			"exit":        exit,
			"priced":      priced,
			"bar_open":    row["open"],
			"bar_close":   row["close"],
			"gap":         row["gap"],
			"gross_bp":    row["gross_bp"],
			"cost_bp":     row["cost_bp"],
			"net_bp":      row["net_bp"],
			"pnl":         pnlOf(row),
			"actual_pnl":  row["actual_pnl"],
			"day_next_bp": nextBP,
			"day_all_bp":  allBP,
			"cause":       causeOf(netBP, allBP),
			"note":        noteOf(row),
		})
	}
	frame := history.NewFrame(TradeSchema, rows)
	return frame.SortBy([]string{"day", "side", "rank"}, []bool{false, false, false})
}

// pricesOf は建値と手仕舞い値。台帳の約定単価があればそれを優先する。
func pricesOf(row map[string]any) (entry, exit any, priced string) {
	actualEntry := floatPtrOf(row["actual_entry"])
	actualExit := floatPtrOf(row["actual_exit"])
	if actualEntry != nil {
		exitValue := any(nil)
		if actualExit != nil {
			exitValue = *actualExit
		}
		return *actualEntry, exitValue, PricedActual
	}
	return row["open"], row["close"], PricedBar
}

// quantityOf は建てた株数。実際の約定があればそれ、無ければ想定の株数。
func quantityOf(row map[string]any) any {
	if v := floatPtrOf(row["filled_quantity"]); v != nil && *v > 0 {
		return *v
	}
	if v := floatPtrOf(row["quantity"]); v != nil {
		return *v
	}
	return row["hypo_quantity"]
}

// pnlOf は円の損益。実現していればそれ、していなければ想定。
func pnlOf(row map[string]any) any {
	if v := floatPtrOf(row["actual_pnl"]); v != nil {
		return *v
	}
	return row["hypo_pnl"]
}

// causeOf は勝敗の主因の当たり。候補全体と同じ向きなら地合い、逆なら選定。
func causeOf(netBP *float64, allBP any) string {
	all, ok := allBP.(float64)
	if netBP == nil || !ok {
		return CauseUnknown
	}
	won := *netBP > 0
	sameDirection := won == (all > 0)
	switch {
	case sameDirection && won:
		return CauseMarketWin
	case sameDirection:
		return CauseMarketLoss
	case won:
		return CauseSelectionWin
	default:
		return CauseSelectionLoss
	}
}

// noteOf は手仕舞いの注意。引けが制限値幅に張り付いていれば、その方向の
// 手仕舞いは約定していない恐れがある（売建はストップ高、買建はストップ安）。
func noteOf(row map[string]any) string {
	side := str(row["side"])
	if side == "SELL" && boolOf(row["limit_up_close"]) {
		return "引けストップ高（返済できず持ち越しの恐れ）"
	}
	if side == "BUY" && boolOf(row["limit_down_close"]) {
		return "引けストップ安（売れず持ち越しの恐れ）"
	}
	return ""
}

// TradeTotalsSchema は明細の合計（脚ごと）。
//
// 勝率と平均だけでは運用の良し悪しは決まらない。同じ勝率でもペイオフ次第で期待値の
// 符号が変わり、同じ損益でも 1 銘柄に依っていれば再現しない。判断に要る材料を並べる。
var TradeTotalsSchema = []history.Column{
	{Name: "side", Type: history.TypeString},
	{Name: "trades", Type: history.TypeInt64},
	{Name: "wins", Type: history.TypeInt64},
	{Name: "win_rate", Type: history.TypeFloat64},
	{Name: "avg_net_bp", Type: history.TypeFloat64},
	{Name: "pnl", Type: history.TypeFloat64},
	// avg_win_bp / avg_loss_bp とその比（payoff）。勝率が低くてもペイオフが高ければ成り立つ。
	{Name: "avg_win_bp", Type: history.TypeFloat64},
	{Name: "avg_loss_bp", Type: history.TypeFloat64},
	{Name: "payoff", Type: history.TypeFloat64},
	// expectancy_bp は 1 取引あたりの期待値（勝率 × 平均利益 − 負率 × 平均損失）。
	{Name: "expectancy_bp", Type: history.TypeFloat64},
	{Name: "best_code", Type: history.TypeString},
	{Name: "best_bp", Type: history.TypeFloat64},
	{Name: "worst_code", Type: history.TypeString},
	{Name: "worst_bp", Type: history.TypeFloat64},
	// top1_share / top3_share は「勝ち取引の利益の合計」に占める上位の割合。
	// 1 発頼みなら高くなる——その 1 発が再現しなければ期間の成績は消える。
	{Name: "top1_share", Type: history.TypeFloat64},
	{Name: "top3_share", Type: history.TypeFloat64},
	// 勝敗 × 主因の 2×2。選定勝ちが選定負けを上回っていれば、順位付けが価値を足している。
	// 地合いの勝ち負けはその日の相場に乗っただけで、規則の証拠にはならない。
	{Name: "market_win", Type: history.TypeInt64},
	{Name: "market_loss", Type: history.TypeInt64},
	{Name: "selection_win", Type: history.TypeInt64},
	{Name: "selection_loss", Type: history.TypeInt64},
	// pinned は引けが制限値幅に張り付いた取引（手仕舞えていない恐れ）。
	{Name: "pinned", Type: history.TypeInt64},
	// slippage_bp は執行の乖離。実際の約定単価と日足の始値・終値の差（bp）。
	// 想定（バックテスト）が寄付・引けなので、この差がそのまま検証との乖離になる。
	{Name: "slippage_bp", Type: history.TypeFloat64},
	// traded は実際に建てた件数（残りは「建てていたら」の想定）。
	{Name: "traded", Type: history.TypeInt64},
}

// TradeTotals は明細を脚ごとにまとめる。
func TradeTotals(trades history.Frame) history.Frame {
	if trades.Height() == 0 {
		return history.NewFrame(TradeTotalsSchema, nil)
	}
	type totals struct {
		trades, wins               int64
		netBP                      []float64
		winBP, lossBP              []float64
		pnl                        float64
		bestCode, worstCode        string
		bestBP, worstBP            float64
		hasBest, hasWorst          bool
		marketWin, marketLoss      int64
		selectionWin, selectionLos int64
		pinned, traded             int64
		slippage                   []float64
	}
	byLeg := map[string]*totals{}
	var order []string
	for _, row := range trades.Rows {
		side := str(row["side"])
		t, ok := byLeg[side]
		if !ok {
			t = &totals{}
			byLeg[side] = t
			order = append(order, side)
		}
		t.trades++
		if v := floatPtrOf(row["pnl"]); v != nil {
			t.pnl += *v
		}
		if str(row["note"]) != "" {
			t.pinned++
		}
		if str(row["priced"]) == PricedActual {
			t.traded++
			if v := slippageOf(row); v != nil {
				t.slippage = append(t.slippage, *v)
			}
		}
		netBP := floatPtrOf(row["net_bp"])
		if netBP == nil {
			continue
		}
		t.netBP = append(t.netBP, *netBP)
		if *netBP > 0 {
			t.wins++
			t.winBP = append(t.winBP, *netBP)
		} else {
			t.lossBP = append(t.lossBP, *netBP)
		}
		switch str(row["cause"]) {
		case CauseMarketWin:
			t.marketWin++
		case CauseMarketLoss:
			t.marketLoss++
		case CauseSelectionWin:
			t.selectionWin++
		case CauseSelectionLoss:
			t.selectionLos++
		}
		label := str(row["code"]) + " " + str(row["name"])
		if !t.hasBest || *netBP > t.bestBP {
			t.bestCode, t.bestBP, t.hasBest = label, *netBP, true
		}
		if !t.hasWorst || *netBP < t.worstBP {
			t.worstCode, t.worstBP, t.hasWorst = label, *netBP, true
		}
	}
	slices.Sort(order)
	rows := make([]map[string]any, 0, len(order))
	for _, side := range order {
		t := byLeg[side]
		avgWin, avgLoss := mean(t.winBP), mean(t.lossBP)
		rows = append(rows, map[string]any{
			"side": side, "trades": t.trades, "wins": t.wins,
			"win_rate":   ratio(t.wins, int64(len(t.netBP))),
			"avg_net_bp": mean(t.netBP), "pnl": t.pnl,
			"avg_win_bp": avgWin, "avg_loss_bp": avgLoss,
			"payoff":        payoffOf(avgWin, avgLoss),
			"expectancy_bp": mean(t.netBP), // 平均 net bp がそのまま 1 取引の期待値
			"best_code":     nilIfEmpty(t.bestCode), "best_bp": floatOrNilIf(t.hasBest, t.bestBP),
			"worst_code": nilIfEmpty(t.worstCode), "worst_bp": floatOrNilIf(t.hasWorst, t.worstBP),
			"top1_share": shareOfTop(t.winBP, 1), "top3_share": shareOfTop(t.winBP, 3),
			"market_win": t.marketWin, "market_loss": t.marketLoss,
			"selection_win": t.selectionWin, "selection_loss": t.selectionLos,
			"pinned": t.pinned, "slippage_bp": mean(t.slippage), "traded": t.traded,
		})
	}
	return history.NewFrame(TradeTotalsSchema, rows)
}

// payoffOf は平均利益 ÷ 平均損失（絶対値）。損失が無ければ nil。
func payoffOf(avgWin, avgLoss any) any {
	win, okWin := avgWin.(float64)
	loss, okLoss := avgLoss.(float64)
	if !okWin || !okLoss || loss == 0 {
		return nil
	}
	return win / -loss
}

// shareOfTop は勝ち取引の利益の合計に占める上位 n 件の割合。
// 1 に近いほど 1 発頼み——その 1 発が再現しなければ期間の成績は消える。
func shareOfTop(winBP []float64, n int) any {
	if len(winBP) == 0 {
		return nil
	}
	sorted := slices.Clone(winBP)
	slices.Sort(sorted)
	slices.Reverse(sorted)
	total, top := 0.0, 0.0
	for i, v := range sorted {
		total += v
		if i < n {
			top += v
		}
	}
	if total == 0 {
		return nil
	}
	return top / total
}

// slippageOf は執行の乖離（bp）。実際の約定が日足の寄付・引けよりどれだけ不利だったか。
// 建て方向で符号を揃える（負なら不利）。約定単価が無い取引は nil。
func slippageOf(row map[string]any) *float64 {
	entry := floatPtrOf(row["entry"])
	exit := floatPtrOf(row["exit"])
	barOpen := floatPtrOf(row["bar_open"])
	barClose := floatPtrOf(row["bar_close"])
	if entry == nil || exit == nil || barOpen == nil || barClose == nil ||
		*barOpen <= 0 || *entry <= 0 {
		return nil
	}
	sign := 1.0
	if str(row["side"]) == "SELL" {
		sign = -1.0
	}
	// 想定（寄付 → 引け）と実際（約定 → 約定）のリターンの差
	want := sign * (*barClose - *barOpen) / *barOpen
	got := sign * (*exit - *entry) / *entry
	diff := (got - want) * 1e4
	return &diff
}

func floatOrNilIf(ok bool, v float64) any {
	if !ok {
		return nil
	}
	return v
}

// DaysOf は明細に出てくる日付（古い順、重複なし）。
func DaysOf(trades history.Frame) []time.Time {
	seen := map[string]bool{}
	var out []time.Time
	for _, row := range trades.Rows {
		day := timeOf(row["day"])
		key := day.Format("2006-01-02")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, day)
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}
