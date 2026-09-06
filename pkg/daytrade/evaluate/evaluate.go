// Package evaluate は候補の結果の評価（daytrade evaluate）と、選定の妥当性の集計（review）。
//
// 朝の順位表（history/ranking）にある全銘柄——選んだものも、次点で見送ったものも——に
// その日の日足（寄付 → 大引）を当て、「建てていたらいくらだったか」を残す。実際に建てた
// ものは台帳の約定と並べる。
//
// 9:00 の順位表が無い日（発注経路を止めていて open が走らない、気配が取れなかった）は、
// 前夜の plan と当日の**始値**から同じ規則で順位を作り直す。これはバックテストと同じ
// 近似で、ranking_source 列で区別する:
//
//   - quotes       … 9:00 の気配で作った順位表（open の記録）
//   - archive_open … 前夜の plan × 当日の始値で作り直した順位表
package evaluate

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtfees "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/fees"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/shopspring/decimal"
)

// NextRows は「次点」として見る順位の幅（N の先の件数）。
const NextRows = 5

// RankGroups は群の並び（表示順）。
var RankGroups = []string{"picked", "next", "rest"}

// Source は順位表の作られ方。
const (
	SourceQuotes      = "quotes"
	SourceArchiveOpen = "archive_open"
)

// Bar はその日の日足（評価に使う分だけ）。
type Bar struct {
	Code  string
	Open  float64
	High  float64
	Low   float64
	Close float64
	// ULFlag / LLFlag は J-Quants のストップ高／安フラグ。
	ULFlag *bool
	LLFlag *bool
	// Midday は前場引け（11:30 までの最後の約定値）。分足のアドオンがある日だけ。
	Midday *float64
}

// RankingRow は順位表の 1 行（history から読んだものも、作り直したものも同じ形）。
type RankingRow struct {
	Side      string
	Rank      int
	Symbol    string
	Code      string
	Name      string
	PrevClose float64
	Price     float64
	Gap       float64
	Vol20     *float64
	Picked    bool
	Quantity  *float64
	Amount    *float64
	N         int
	Budget    float64
}

// EvaluationSchema は評価結果 1 行の列。
var EvaluationSchema = []history.Column{
	// ranking_source は quotes（9:00 の気配）/ archive_open（plan × 始値で作り直し）。
	{Name: "ranking_source", Type: history.TypeString},
	// ranking_run_id は元にした順位表の run_id（作り直しなら null）。
	{Name: "ranking_run_id", Type: history.TypeString},
	{Name: "side", Type: history.TypeString},
	{Name: "rank", Type: history.TypeInt64},
	{Name: "symbol", Type: history.TypeString},
	{Name: "code", Type: history.TypeString},
	{Name: "name", Type: history.TypeString},
	// rank_group は picked（選んだ）/ next（N の先 NextRows 件）/ rest（それ以外の候補）。
	{Name: "rank_group", Type: history.TypeString},
	{Name: "prev_close", Type: history.TypeFloat64},
	// price / gap は順位付けに使った値（quotes なら 9:00 の気配、archive_open なら始値）。
	{Name: "price", Type: history.TypeFloat64},
	{Name: "gap", Type: history.TypeFloat64},
	{Name: "vol20", Type: history.TypeFloat64},
	{Name: "picked", Type: history.TypeBool},
	{Name: "quantity", Type: history.TypeFloat64},
	{Name: "amount", Type: history.TypeFloat64},
	{Name: "n", Type: history.TypeInt64},
	{Name: "budget", Type: history.TypeFloat64},
	{Name: "open", Type: history.TypeFloat64},
	{Name: "high", Type: history.TypeFloat64},
	{Name: "low", Type: history.TypeFloat64},
	{Name: "close", Type: history.TypeFloat64},
	// gap_open は実際の寄付ギャップ。quotes の gap との差が気配の当たり具合。
	{Name: "gap_open", Type: history.TypeFloat64},
	// ret_oc は寄付 → 大引のリターン（符号は建て方向で見る前）。
	{Name: "ret_oc", Type: history.TypeFloat64},
	// gross_bp は費用前、net_bp は費用（滑り・貸株料等）後の損益（bp）。
	{Name: "gross_bp", Type: history.TypeFloat64},
	{Name: "cost_bp", Type: history.TypeFloat64},
	{Name: "net_bp", Type: history.TypeFloat64},
	// midday は前場引け（11:30 までの最後の約定値。分足がある日だけ）、
	// midday_* はそこで手仕舞ったときの損益。ロングの利益は前場で出尽くし、
	// 前倒しすると MaxDD が半分になる（研究ノート 2026-09-jp-gap-minute の発見 2）——
	// 既定の出口は 15:20 のまま、毎日この差を測って効きが続くかを見る。
	{Name: "midday", Type: history.TypeFloat64},
	{Name: "midday_gross_bp", Type: history.TypeFloat64},
	{Name: "midday_net_bp", Type: history.TypeFloat64},
	// hypo_* は「建てていたら」の株数と円損益。
	{Name: "hypo_quantity", Type: history.TypeFloat64},
	{Name: "hypo_pnl", Type: history.TypeFloat64},
	// limit_*_close は大引がストップ高／安（売建は返済が約定せず持ち越す）。
	{Name: "limit_up_close", Type: history.TypeBool},
	{Name: "limit_down_close", Type: history.TypeBool},
	{Name: "ul_flag", Type: history.TypeBool},
	{Name: "ll_flag", Type: history.TypeBool},
	// traded / filled_quantity / actual_pnl は台帳の本発注の実績。
	{Name: "traded", Type: history.TypeBool},
	{Name: "filled_quantity", Type: history.TypeFloat64},
	// actual_entry / actual_exit は実際の約定単価（建て／手仕舞い）。日足の始値・終値とは
	// 別物で、滑りと執行の巧拙はこの差に出る。手仕舞い前なら actual_exit は null。
	{Name: "actual_entry", Type: history.TypeFloat64},
	{Name: "actual_exit", Type: history.TypeFloat64},
	{Name: "actual_pnl", Type: history.TypeFloat64},
}

// BarsFor はその日の日足。
func BarsFor(arch *archive.Archive, day time.Time) (map[string]Bar, error) {
	source, ok := archsql.Source(arch, universe.EPBars, day, day)
	if !ok {
		return map[string]Bar{}, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	// UL / LL 列を持たない古いアーカイブでも読めるよう、列の有無で問い合わせを変える
	flags := `NULL::BOOLEAN AS ul_flag, NULL::BOOLEAN AS ll_flag`
	if hasColumn(db, source, "UL") {
		flags = `CAST("UL" AS VARCHAR) = '1' AS ul_flag, CAST("LL" AS VARCHAR) = '1' AS ll_flag`
	}
	query := fmt.Sprintf(`
SELECT CAST("Code" AS VARCHAR) AS code,
       TRY_CAST("O" AS DOUBLE) AS o, TRY_CAST("H" AS DOUBLE) AS h,
       TRY_CAST("L" AS DOUBLE) AS l, TRY_CAST("C" AS DOUBLE) AS c,
       %s
FROM %s
WHERE "Date" = %s AND TRY_CAST("O" AS DOUBLE) > 0 AND TRY_CAST("C" AS DOUBLE) > 0`,
		flags, source, archsql.Lit(day))
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("日足の読み出しに失敗しました: %w", err)
	}
	defer rows.Close()
	out := map[string]Bar{}
	for rows.Next() {
		var (
			code           string
			o, h, l, c     sql.NullFloat64
			ulFlag, llFlag sql.NullBool
		)
		if err := rows.Scan(&code, &o, &h, &l, &c, &ulFlag, &llFlag); err != nil {
			return nil, err
		}
		bar := Bar{Code: code, Open: o.Float64, High: h.Float64, Low: l.Float64, Close: c.Float64}
		if ulFlag.Valid {
			v := ulFlag.Bool
			bar.ULFlag = &v
		}
		if llFlag.Valid {
			v := llFlag.Bool
			bar.LLFlag = &v
		}
		out[code] = bar
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 前場引けは分足（アドオン）から。取れない日は nil のまま——評価は日足だけで成り立つ
	midday, err := MiddayFor(arch, day)
	if err != nil {
		return nil, err
	}
	for code, value := range midday {
		if bar, ok := out[code]; ok {
			v := value
			bar.Midday = &v
			out[code] = bar
		}
	}
	return out, nil
}

// MiddayFor はその日の前場引け（11:30 までの最後の約定値）。
//
// 分足のアドオンが無い日・取り込んでいない日は空を返す（エラーにしない）。
func MiddayFor(arch *archive.Archive, day time.Time) (map[string]float64, error) {
	source, ok := archsql.Source(arch, universe.EPMinute, day, day)
	if !ok {
		return map[string]float64{}, nil
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := fmt.Sprintf(`
SELECT CAST("Code" AS VARCHAR) AS code, arg_max("C", "Time") AS midday
FROM %s
WHERE "Date" = %s AND "Time" <= '%s'
GROUP BY 1`, source, archsql.Lit(day), MorningClose)
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("分足の読み出しに失敗しました: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var code string
		var value sql.NullFloat64
		if err := rows.Scan(&code, &value); err != nil {
			return nil, err
		}
		if value.Valid && value.Float64 > 0 {
			out[code] = value.Float64
		}
	}
	return out, rows.Err()
}

// MorningClose は前場の引け（この足に前場の板寄せが入る）。
const MorningClose = "11:30"

func hasColumn(db *sql.DB, source, column string) bool {
	var n int
	query := fmt.Sprintf(`SELECT count(*) FROM (DESCRIBE SELECT * FROM %s) WHERE column_name = '%s'`, source, column)
	if err := db.QueryRow(query).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ReconstructRanking は前夜の plan と当日の始値から、open と同じ規則で順位表を作り直す。
//
// 9:00 の気配の代わりに始値を使う（バックテストと同じ近似）。ロングは N・予算・配分を
// [capital] から、ショートは [margin] の通常日の倍率で。
func ReconstructRanking(p plan.Plan, bars map[string]Bar, cfg config.Config, at time.Time) []RankingRow {
	quotes := map[string]selection.Quote{}
	for _, c := range p.Candidates {
		bar, ok := bars[c.Code]
		if !ok || bar.Open <= 0 {
			continue
		}
		quotes[c.Symbol] = selection.Quote{
			Symbol: c.Symbol, Price: decimal.NewFromFloat(bar.Open),
			At: at, Source: SourceArchiveOpen,
		}
	}
	n := cfg.Capital.Positions()
	budget := cfg.Capital.BudgetPerOrder()

	// ショートを先に決め、余りをロングに回す（open と同じ順序。危険信号・ショックは
	// 作り直しでは見ない——前夜の plan と始値だけから同じ規則で作るため）
	var shortRows []RankingRow
	if cfg.Margin.Enabled && cfg.Margin.Positions() > 0 {
		shortN := cfg.Margin.Positions()
		shortBudget := cfg.Margin.BudgetPerOrder().Mul(cfg.Margin.MultiplierNormal).Round(0)
		shortRanking := selection.RankShort(p.ShortEligible(), quotes, cfg.Margin)
		shortPicks := selection.PickFrom(shortRanking, selection.PickOptions{
			N: shortN, Budget: shortBudget, Weighting: cfg.Margin.Weighting, Side: domain.SideSell,
			MaxAmount: cfg.Margin.MaxOrder,
		})
		shortRows = rowsOf(shortRanking, shortPicks, "SELL", shortN, shortBudget)
		if cfg.Margin.SpillToLong {
			used := decimal.Zero
			for _, pk := range shortPicks {
				used = used.Add(pk.Amount())
			}
			spill := shortBudget.Mul(decimal.NewFromInt(int64(shortN))).Sub(used)
			n, budget = selection.SpillInto(n, budget, cfg.Capital.BudgetPerOrder(), spill, cfg.Capital.MaxPositions)
		}
	}
	longRanking := selection.Rank(p.Eligible(), quotes, cfg.Signal)
	longPicks := selection.PickFrom(longRanking, selection.PickOptions{
		N: n, Budget: budget, Weighting: cfg.Capital.Weighting, Side: domain.SideBuy,
	})
	return append(rowsOf(longRanking, longPicks, "BUY", n, budget), shortRows...)
}

func rowsOf(ranking []selection.Ranked, picks []selection.Pick, side string, n int, budget decimal.Decimal) []RankingRow {
	picked := map[string]selection.Pick{}
	for _, p := range picks {
		picked[p.Symbol] = p
	}
	budgetF, _ := budget.Float64()
	out := make([]RankingRow, 0, len(ranking))
	for _, r := range ranking {
		prevClose, _ := r.PrevClose.Float64()
		price, _ := r.Price.Float64()
		gap, _ := r.Gap.Float64()
		row := RankingRow{
			Side: side, Rank: r.Rank, Symbol: r.Symbol, Code: r.Code, Name: r.Name,
			PrevClose: prevClose, Price: price, Gap: gap, Vol20: r.Vol,
			N: n, Budget: budgetF,
		}
		if p, ok := picked[r.Symbol]; ok {
			quantity, _ := p.Quantity.Float64()
			amount, _ := p.Amount().Float64()
			row.Picked = true
			row.Quantity = &quantity
			row.Amount = &amount
		}
		out = append(out, row)
	}
	return out
}

// RowsFromFrame は history の ranking を RankingRow に読み直す。
func RowsFromFrame(frame history.Frame) (rows []RankingRow, runID string) {
	for _, raw := range frame.Rows {
		if runID == "" {
			runID = str(raw["run_id"])
		}
		rows = append(rows, RankingRow{
			Side: str(raw["side"]), Rank: int(intOf(raw["rank"])), Symbol: str(raw["symbol"]),
			Code: str(raw["code"]), Name: str(raw["name"]),
			PrevClose: floatOf(raw["prev_close"]), Price: floatOf(raw["price"]),
			Gap: floatOf(raw["gap"]), Vol20: floatPtrOf(raw["vol20"]),
			Picked: boolOf(raw["picked"]), Quantity: floatPtrOf(raw["quantity"]),
			Amount: floatPtrOf(raw["amount"]),
			N:      int(intOf(raw["n"])), Budget: floatOf(raw["budget"]),
		})
	}
	return rows, runID
}

// actual は台帳の本発注から (銘柄, 脚) → (約定数量, 約定単価, 実現損益)。
type actual struct {
	filled float64
	// entry / exit は約定単価（建て／手仕舞い）。手仕舞い前なら exit は nil。
	entry *float64
	exit  *float64
	pnl   *float64
}

func actualsOf(orders []dtledger.Order) map[string]actual {
	entries := map[string]dtledger.Order{}
	exits := map[string]dtledger.Order{}
	for _, o := range orders {
		if o.IsDryRun() || o.IsDead() {
			continue
		}
		// 実機検証の注文は成績ではない（docs/BROKER_VERIFY.md）。候補の評価から外す
		if o.Verify {
			continue
		}
		key := o.Symbol + "|" + o.Leg()
		if o.IsEntry() {
			entries[key] = o
		} else {
			exits[key] = o
		}
	}
	out := map[string]actual{}
	for key, entry := range entries {
		filled, _ := entry.FilledQuantity.Float64()
		if filled <= 0 {
			continue
		}
		a := actual{filled: filled}
		if entry.AvgFillPrice != nil {
			price, _ := entry.AvgFillPrice.Float64()
			a.entry = &price
		}
		if exit, ok := exits[key]; ok && exit.AvgFillPrice != nil {
			price, _ := exit.AvgFillPrice.Float64()
			a.exit = &price
			if entry.AvgFillPrice != nil {
				buy, sell := *entry.AvgFillPrice, *exit.AvgFillPrice
				if entry.Side != domain.SideBuy {
					buy, sell = *exit.AvgFillPrice, *entry.AvgFillPrice
				}
				pnl, _ := sell.Sub(buy).Mul(exit.FilledQuantity).Float64()
				a.pnl = &pnl
			}
		}
		out[key] = a
	}
	return out
}

// costBP は往復の費用（bp）。信用は設定の見込み値、現物は手数料込みの実測式。
func costBP(side string, amount float64, cfg config.Config) float64 {
	if side == "SELL" {
		f, _ := cfg.Margin.ExtraCostBP.Float64()
		return f
	}
	if cfg.Margin.Enabled && cfg.Margin.LongViaMargin {
		f, _ := cfg.Margin.LongExtraCostBP.Float64()
		return f
	}
	f, _ := dtfees.RoundTripBP(decimal.NewFromFloat(amount)).Float64()
	return f
}

// Evaluate は順位表の全行に日足と台帳を当てる。
// 日足が無い銘柄は結果の列が null のまま残る（「なぜ評価できないか」を残すため）。
func Evaluate(ranking []RankingRow, runID string, bars map[string]Bar, cfg config.Config, orders []dtledger.Order, source string) history.Frame {
	actuals := actualsOf(orders)
	rows := make([]map[string]any, 0, len(ranking))
	for _, r := range ranking {
		sign := 1.0
		if r.Side == "SELL" {
			sign = -1.0
		}
		group := "rest"
		switch {
		case r.Picked:
			group = "picked"
		case r.Rank <= r.N+NextRows:
			group = "next"
		}
		row := map[string]any{
			"ranking_source": source,
			"ranking_run_id": nilIfEmpty(runID),
			"side":           r.Side,
			"rank":           int64(r.Rank),
			"symbol":         r.Symbol,
			"code":           r.Code,
			"name":           r.Name,
			"rank_group":     group,
			"prev_close":     r.PrevClose,
			"price":          r.Price,
			"gap":            r.Gap,
			"vol20":          floatOrNil(r.Vol20),
			"picked":         r.Picked,
			"quantity":       floatOrNil(r.Quantity),
			"amount":         floatOrNil(r.Amount),
			"n":              int64(r.N),
			"budget":         r.Budget,
		}
		for _, name := range []string{"open", "high", "low", "close", "gap_open", "ret_oc",
			"gross_bp", "cost_bp", "net_bp", "hypo_quantity", "hypo_pnl",
			"midday", "midday_gross_bp", "midday_net_bp"} {
			row[name] = nil
		}
		row["ul_flag"], row["ll_flag"] = nil, nil
		row["limit_up_close"], row["limit_down_close"] = nil, nil

		bar, hasBar := bars[r.Code]
		if hasBar {
			row["open"], row["high"] = bar.Open, bar.High
			row["low"], row["close"] = bar.Low, bar.Close
			row["ul_flag"], row["ll_flag"] = boolOrNil(bar.ULFlag), boolOrNil(bar.LLFlag)
		}
		if hasBar && bar.Open > 0 && bar.Close > 0 && r.PrevClose > 0 {
			low, high, _ := marketrules.PriceLimitRange(decimal.NewFromFloat(r.PrevClose))
			lowF, _ := low.Float64()
			highF, _ := high.Float64()
			ret := bar.Close/bar.Open - 1
			gross := sign * ret * 1e4
			quantity := 0.0
			if r.Picked && r.Quantity != nil {
				quantity = *r.Quantity
			} else {
				q := selection.SharesFor(decimal.NewFromFloat(r.Budget),
					decimal.NewFromFloat(bar.Open), marketrules.DefaultLotSize)
				quantity, _ = q.Float64()
			}
			amount := quantity * bar.Open
			if r.Amount != nil {
				amount = *r.Amount
			}
			cost := costBP(r.Side, amount, cfg)
			row["gap_open"] = bar.Open/r.PrevClose - 1
			row["ret_oc"] = ret
			row["gross_bp"] = gross
			row["cost_bp"] = cost
			row["net_bp"] = gross - cost
			row["hypo_quantity"] = quantity
			row["hypo_pnl"] = quantity*sign*(bar.Close-bar.Open) - quantity*bar.Open*cost/1e4
			// 浮動小数の丸めで制限値幅をわずかに外すことがあるので余裕を持たせる
			row["limit_up_close"] = bar.Close >= highF-1e-6
			row["limit_down_close"] = bar.Close <= lowF+1e-6
			// 前場引けで手仕舞っていたら（出口を前倒しする案の材料）
			if bar.Midday != nil && *bar.Midday > 0 {
				middayGross := sign * (*bar.Midday/bar.Open - 1) * 1e4
				row["midday"] = *bar.Midday
				row["midday_gross_bp"] = middayGross
				row["midday_net_bp"] = middayGross - cost
			}
		}
		leg := "long"
		if r.Side == "SELL" {
			leg = "short"
		}
		if a, ok := actuals[r.Symbol+"|"+leg]; ok {
			row["traded"] = true
			row["filled_quantity"] = a.filled
			row["actual_entry"] = floatOrNil(a.entry)
			row["actual_exit"] = floatOrNil(a.exit)
			row["actual_pnl"] = floatOrNil(a.pnl)
		} else {
			row["traded"] = false
			row["filled_quantity"] = nil
			row["actual_entry"], row["actual_exit"] = nil, nil
			row["actual_pnl"] = nil
		}
		rows = append(rows, row)
	}
	frame := history.NewFrame(EvaluationSchema, rows)
	return frame.SortBy([]string{"side", "rank"}, []bool{false, false})
}

func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func floatOrNil(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func boolOrNil(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}
