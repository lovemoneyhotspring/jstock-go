package margincap

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

// Result は適用の結果。ログとダイジェストに出すためのもの。
type Result struct {
	// Applied はどちらかの脚を下げたか。
	Applied bool
	// Shortfall は不足額（追証）が出ていたか。出ていれば建ててはいけない。
	Shortfall bool
	// WatchOnly は適用の結果 N が 0 に落ちたか。**落ちたら黙って続けてはいけない**。
	WatchOnly bool
	// Long / Short は脚ごとの before → after。
	Long, Short LegChange
}

// LegChange は 1 脚ぶんの変化。
type LegChange struct {
	Before, After decimal.Decimal
	// N は銘柄数。適用の前後で変わってはいけない（変わったら設計の誤り）。
	NBefore, NAfter int
}

// Changed は実際に下がったか。
func (c LegChange) Changed() bool { return !c.Before.Equal(c.After) }

// Fields はログ用。
func (r Result) Fields() map[string]any {
	return map[string]any{
		"applied":     r.Applied,
		"shortfall":   r.Shortfall,
		"watch_only":  r.WatchOnly,
		"long_before": r.Long.Before.StringFixed(0), "long_after": r.Long.After.StringFixed(0),
		"short_before": r.Short.Before.StringFixed(0), "short_after": r.Short.After.StringFixed(0),
		"n_long": r.Long.NAfter, "n_short": r.Short.NAfter,
	}
}

// Apply は保証金から導いた上限を設定に反映する。**下げ方向のみ**。
//
// 設定が狙いの水準で、ここは「その日それが建てられるか」の検算。保証金が増えていても
// 勝手には上げない——増えたぶんを使うかは人が決める（package のコメント）。
//
// max_capital を下げるときは order_budget も max_capital ÷ N で置き直す。
// N = round(max_capital ÷ order_budget) なので、片方だけ下げると N が変わり、
// 最悪 0 に落ちて open が watch-only（その朝は何も建てない）に転ぶ。
func Apply(cfg config.Config, s Snapshot) (config.Config, Result) {
	res := Result{Shortfall: s.Fusokugaku.GreaterThan(decimal.Zero)}
	derivedLong, derivedShort := legTargets(cfg, s.Capacity())

	res.Long = shrinkLong(&cfg, derivedLong)
	res.Short = shrinkShort(&cfg, derivedShort)
	res.Applied = res.Long.Changed() || res.Short.Changed()
	res.WatchOnly = cfg.Capital.Positions() == 0 && (cfg.Margin.Positions() == 0)
	return cfg, res
}

// legTargets は建玉合計を脚ごとに割る。比は**設定の max_capital そのまま**。
//
// 比をここに持たないのは、長短比が設定側の判断だから。固定値（例 6:4）を置くと、
// 保証金が減った日だけ設定と違う比に引き戻される——縮小はリスクを下げる操作であって、
// 長短の方針を変える操作ではない。
//
// ショートが無効・0 ならすべてロングへ。両脚とも 0 なら割りようがないので 0 を返す
// （呼び出し側は「下げ方向のみ」なので、0 と比べれば何も起きない）。
func legTargets(cfg config.Config, capacity decimal.Decimal) (long, short decimal.Decimal) {
	longCap := cfg.Capital.MaxCapital
	shortCap := decimal.Zero
	if cfg.Margin.Enabled {
		shortCap = cfg.Margin.MaxCapital
	}
	total := longCap.Add(shortCap)
	if !total.IsPositive() || !capacity.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	long = capacity.Mul(longCap).Div(total).Floor()
	return long, capacity.Sub(long)
}

func shrinkLong(cfg *config.Config, derived decimal.Decimal) LegChange {
	c := LegChange{Before: cfg.Capital.MaxCapital, After: cfg.Capital.MaxCapital}
	c.NBefore = cfg.Capital.Positions()
	c.NAfter = c.NBefore
	// 資金 0（様子見モード）はそのまま。N が出ない設定に触らない
	if c.NBefore == 0 || !derived.LessThan(cfg.Capital.MaxCapital) {
		return c
	}
	ratio := derived.Div(cfg.Capital.MaxCapital)
	cfg.Capital.MaxCapital = derived.Floor()
	cfg.Capital.OrderBudget = scaleBudget(derived, c.NBefore)
	cfg.Capital.MaxOrder = scaleCap(cfg.Capital.MaxOrder, ratio)
	c.After = cfg.Capital.MaxCapital
	c.NAfter = cfg.Capital.Positions()
	return c
}

func shrinkShort(cfg *config.Config, derived decimal.Decimal) LegChange {
	c := LegChange{Before: cfg.Margin.MaxCapital, After: cfg.Margin.MaxCapital}
	c.NBefore = cfg.Margin.Positions()
	c.NAfter = c.NBefore
	if !cfg.Margin.Enabled || c.NBefore == 0 || !derived.LessThan(cfg.Margin.MaxCapital) {
		return c
	}
	ratio := derived.Div(cfg.Margin.MaxCapital)
	cfg.Margin.MaxCapital = derived.Floor()
	cfg.Margin.OrderBudget = scaleBudget(derived, c.NBefore)
	cfg.Margin.MaxOrder = scaleCap(cfg.Margin.MaxOrder, ratio)
	c.After = cfg.Margin.MaxCapital
	c.NAfter = cfg.Margin.Positions()
	return c
}

// scaleBudget は N を保つ 1 注文の目安（max_capital ÷ N）。1 円未満にはしない
// （order_budget は正の値でなければ Validate が落ちる）。
func scaleBudget(capital decimal.Decimal, n int) decimal.Decimal {
	if n < 1 {
		return capital
	}
	budget := capital.Div(decimal.NewFromInt(int64(n))).Floor()
	if budget.LessThanOrEqual(decimal.Zero) {
		return decimal.NewFromInt(1)
	}
	return budget
}

// scaleCap は 1 銘柄の上限を同率で縮める。0 は「上限なし」なので触らない。
func scaleCap(cap, ratio decimal.Decimal) decimal.Decimal {
	if cap.IsZero() {
		return cap
	}
	return cap.Mul(ratio).Floor()
}

// Describe は適用の結果を 1 行で。
func (r Result) Describe() string {
	if !r.Applied {
		return "保証金による縮小なし（設定の水準で建てられる）"
	}
	return fmt.Sprintf("保証金で縮小: ロング %s → %s / ショート %s → %s",
		r.Long.Before.StringFixed(0), r.Long.After.StringFixed(0),
		r.Short.Before.StringFixed(0), r.Short.After.StringFixed(0))
}
