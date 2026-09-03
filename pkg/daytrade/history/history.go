// Package history はデイトレの選定履歴。前夜の候補、9:00 の気配・順位表・選定、
// 実行の要約を積む。
//
// 置き場は state/daytrade/history/<kind>/（wbcore/history.Store）。1 回の plan / open が
// 1 ファイルで、上書きしない。cron が 9:01・9:04・9:07 と 3 回 open を叩けば 3 ファイル
// 残り、run_id で区別できる。
//
//	kind        | 1 行                                        | いつ
//	------------|---------------------------------------------|---------------------
//	plan        | 母集団の 1 銘柄（除外理由の列ごと）              | plan のたび
//	plan_meta   | plan 1 回の要約（件数・信号）                   | plan のたび
//	quotes      | 9:00 に受け取った気配 1 銘柄（使えたかの印付き）   | open が気配を取ったとき
//	ranking     | 順位表 1 行（ロング・ショート両方、選定の印付き）  | open が順位を付けたとき
//	open_run    | open 1 回の要約（危険信号・件数・結末）           | open が判断まで進んだとき
//	evaluation  | 候補 1 行に当日の日足と台帳を当てた結果            | evaluate のたび
//
// 順位表は JSONL ログと違い**全行**を持つ。「なぜ X が選ばれなかったか」は ranking に
// 無ければ quotes（ギャップが条件外・ストップ安・気配なし）で追える。
package history

import (
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
)

// 種類の名前。
const (
	KindPlan       = "plan"
	KindPlanMeta   = "plan_meta"
	KindQuotes     = "quotes"
	KindRanking    = "ranking"
	KindOpenRun    = "open_run"
	KindEvaluation = "evaluation"
)

// PlanSchema は母集団 1 銘柄の列。
var PlanSchema = []history.Column{
	{Name: "Code", Type: history.TypeString},
	{Name: "symbol", Type: history.TypeString},
	{Name: "name", Type: history.TypeString},
	{Name: "segment", Type: history.TypeString},
	{Name: "prev_close", Type: history.TypeFloat64},
	{Name: "turnover_med", Type: history.TypeFloat64},
	{Name: "mkt_cap", Type: history.TypeFloat64},
	{Name: "vol20", Type: history.TypeFloat64},
	{Name: "cap_tercile", Type: history.TypeInt64},
	{Name: "earn_prev", Type: history.TypeBool},
	{Name: "disc_today", Type: history.TypeBool},
	{Name: "alert", Type: history.TypeBool},
	{Name: "jsf_stop", Type: history.TypeBool},
	{Name: "shortable", Type: history.TypeBool},
	{Name: "eligible", Type: history.TypeBool},
	{Name: "short_eligible", Type: history.TypeBool},
}

// PlanMetaSchema は plan 1 回の要約。
var PlanMetaSchema = []history.Column{
	{Name: "prev_day", Type: history.TypeDate},
	{Name: "positions", Type: history.TypeInt64},
	{Name: "budget_per_order", Type: history.TypeFloat64},
	{Name: "iv_prev", Type: history.TypeFloat64},
	{Name: "iv_gate", Type: history.TypeFloat64},
	{Name: "drift", Type: history.TypeFloat64},
	{Name: "candidates", Type: history.TypeInt64},
	{Name: "eligible", Type: history.TypeInt64},
	{Name: "short_eligible", Type: history.TypeInt64},
	{Name: "created_at", Type: history.TypeString},
}

// QuotesSchema は受け取った気配 1 銘柄。
var QuotesSchema = []history.Column{
	{Name: "symbol", Type: history.TypeString},
	{Name: "price", Type: history.TypeFloat64},
	{Name: "quote_at", Type: history.TypeTimestamp},
	{Name: "source", Type: history.TypeString},
	{Name: "delayed", Type: history.TypeBool},
	// usable は鮮度の検査を通り、順位付けに使えた気配か。
	{Name: "usable", Type: history.TypeBool},
	{Name: "prev_close", Type: history.TypeFloat64},
	{Name: "gap", Type: history.TypeFloat64},
}

// RankingSchema は順位表 1 行。
var RankingSchema = []history.Column{
	// side は BUY（ロング: ギャップ下位）/ SELL（ショート: ギャップ上位）。
	{Name: "side", Type: history.TypeString},
	{Name: "rank", Type: history.TypeInt64},
	{Name: "symbol", Type: history.TypeString},
	{Name: "code", Type: history.TypeString},
	{Name: "name", Type: history.TypeString},
	{Name: "prev_close", Type: history.TypeFloat64},
	{Name: "price", Type: history.TypeFloat64},
	{Name: "gap", Type: history.TypeFloat64},
	{Name: "vol20", Type: history.TypeFloat64},
	{Name: "picked", Type: history.TypeBool},
	{Name: "quantity", Type: history.TypeFloat64},
	{Name: "amount", Type: history.TypeFloat64},
	{Name: "n", Type: history.TypeInt64},
	{Name: "budget", Type: history.TypeFloat64},
}

// OpenRunSchema は open 1 回の要約。
var OpenRunSchema = []history.Column{
	// mode は live / dry_run / watch（資金 0）。
	{Name: "mode", Type: history.TypeString},
	// outcome は picked / regime / no_quotes / no_picks / no_capital。
	{Name: "outcome", Type: history.TypeString},
	{Name: "quotes_requested", Type: history.TypeInt64},
	{Name: "quotes_received", Type: history.TypeInt64},
	{Name: "quotes_usable", Type: history.TypeInt64},
	{Name: "trade", Type: history.TypeBool},
	{Name: "reasons", Type: history.TypeString},
	{Name: "scale", Type: history.TypeFloat64},
	{Name: "weak", Type: history.TypeBool},
	{Name: "iv_prev", Type: history.TypeFloat64},
	{Name: "drift_bp", Type: history.TypeFloat64},
	{Name: "market_gap_bp", Type: history.TypeFloat64},
	{Name: "recent_pnl", Type: history.TypeFloat64},
	{Name: "us_ret_bp", Type: history.TypeFloat64},
	{Name: "vix", Type: history.TypeFloat64},
	{Name: "n", Type: history.TypeInt64},
	{Name: "budget", Type: history.TypeFloat64},
	{Name: "weighting", Type: history.TypeString},
	{Name: "short_n", Type: history.TypeInt64},
	{Name: "short_budget", Type: history.TypeFloat64},
	{Name: "short_multiplier", Type: history.TypeFloat64},
	{Name: "long_picks", Type: history.TypeInt64},
	{Name: "short_picks", Type: history.TypeInt64},
	// orders は台帳に書いた注文の数（dry-run を含む）、failures は通らなかった数。
	{Name: "orders", Type: history.TypeInt64},
	{Name: "failures", Type: history.TypeInt64},
}

// StoreFor はこの環境の履歴ストア。
func StoreFor(s *settings.AppSettings) *history.Store {
	return history.NewStore(s.DaytradeHistoryDir())
}

// PlanFrames は plan の母集団（全銘柄）と要約（1 行）。
func PlanFrames(p plan.Plan) (frame, meta history.Frame) {
	rows := make([]map[string]any, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		rows = append(rows, map[string]any{
			"Code": c.Code, "symbol": c.Symbol, "name": c.Name, "segment": c.Segment,
			"prev_close": c.PrevClose, "turnover_med": c.TurnoverMed, "mkt_cap": c.MktCap,
			"vol20": floatOrNil(c.Vol20), "cap_tercile": int64(c.CapTercile),
			"earn_prev": c.EarnPrev, "disc_today": c.DiscToday, "alert": c.Alert,
			"jsf_stop": c.JsfStop, "shortable": c.Shortable,
			"eligible": c.Eligible, "short_eligible": c.ShortEligible,
		})
	}
	prevDay, _ := time.Parse(plan.DateLayout, p.Meta.PrevDay)
	metaRow := map[string]any{
		"prev_day":         prevDay,
		"positions":        int64(p.Meta.Positions),
		"budget_per_order": parseFloat(p.Meta.BudgetPerOrder),
		"iv_prev":          floatOrNil(p.Meta.IVPrev),
		"iv_gate":          parseFloat(p.Meta.IVGate),
		"drift":            floatOrNil(p.Meta.Drift),
		"candidates":       int64(p.Meta.Candidates),
		"eligible":         int64(p.Meta.Eligible),
		"short_eligible":   int64(p.Meta.ShortEligible),
		"created_at":       p.Meta.CreatedAt,
	}
	return history.NewFrame(PlanSchema, rows),
		history.NewFrame(PlanMetaSchema, []map[string]any{metaRow})
}

// QuotesFrame は受け取った気配の全件。usable は鮮度の検査を通ったもの。
func QuotesFrame(received map[string]selection.Quote, usable map[string]selection.Quote, prevClose map[string]float64) history.Frame {
	symbols := make([]string, 0, len(received))
	for symbol := range received {
		symbols = append(symbols, symbol)
	}
	sortStrings(symbols)
	rows := make([]map[string]any, 0, len(symbols))
	for _, symbol := range symbols {
		q := received[symbol]
		price, _ := q.Price.Float64()
		_, isUsable := usable[symbol]
		row := map[string]any{
			"symbol": symbol, "price": price,
			"quote_at": clock.EnsureUTC(q.At), "source": q.Source, "delayed": q.Delayed,
			"usable": isUsable, "prev_close": nil, "gap": nil,
		}
		if prev, ok := prevClose[symbol]; ok && prev > 0 {
			row["prev_close"] = prev
			row["gap"] = price/prev - 1
		}
		rows = append(rows, row)
	}
	return history.NewFrame(QuotesSchema, rows)
}

// RankingFrame は順位表の全行に、選ばれた銘柄の株数・金額を付ける。
func RankingFrame(ranking []selection.Ranked, picks []selection.Pick, side string, n int, budget decimal.Decimal) history.Frame {
	picked := make(map[string]selection.Pick, len(picks))
	for _, p := range picks {
		picked[p.Symbol] = p
	}
	budgetF, _ := budget.Float64()
	rows := make([]map[string]any, 0, len(ranking))
	for _, r := range ranking {
		prevClose, _ := r.PrevClose.Float64()
		price, _ := r.Price.Float64()
		gap, _ := r.Gap.Float64()
		row := map[string]any{
			"side": side, "rank": int64(r.Rank), "symbol": r.Symbol, "code": r.Code,
			"name": r.Name, "prev_close": prevClose, "price": price, "gap": gap,
			"vol20": floatOrNil(r.Vol), "picked": false,
			"quantity": nil, "amount": nil,
			"n": int64(n), "budget": budgetF,
		}
		if p, ok := picked[r.Symbol]; ok {
			quantity, _ := p.Quantity.Float64()
			amount, _ := p.Amount().Float64()
			row["picked"] = true
			row["quantity"] = quantity
			row["amount"] = amount
		}
		rows = append(rows, row)
	}
	return history.NewFrame(RankingSchema, rows)
}

// OpenRunFrame は open 1 回の要約。OpenRunSchema に無い項目は捨てる。
func OpenRunFrame(fields map[string]any) history.Frame {
	row := make(map[string]any, len(OpenRunSchema))
	for _, column := range OpenRunSchema {
		value, ok := fields[column.Name]
		if !ok {
			row[column.Name] = nil
			continue
		}
		row[column.Name] = coerce(value, column.Type)
	}
	return history.NewFrame(OpenRunSchema, []map[string]any{row})
}

// coerce は列の型に値を寄せる。型が揺れると DuckDB の union_by_name が破綻する。
func coerce(value any, columnType history.ColumnType) any {
	if value == nil {
		return nil
	}
	switch columnType {
	case history.TypeFloat64:
		return history.ToFloat(value)
	case history.TypeInt64:
		return history.ToInt(value)
	default:
		return value
	}
}

func floatOrNil(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func parseFloat(text string) any {
	d, err := decimal.NewFromString(text)
	if err != nil {
		return nil
	}
	f, _ := d.Float64()
	return f
}
