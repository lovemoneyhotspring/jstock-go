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
	// Ratio は margin.capacity_ratio で上限を決め直したか。NormalTotal / ShockTotal はそのときの
	// 長短合計の上限（平日・ショック日）。
	Ratio                   bool
	NormalTotal, ShockTotal decimal.Decimal
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
		"ratio": r.Ratio, "normal_total": r.NormalTotal.StringFixed(0), "shock_total": r.ShockTotal.StringFixed(0),
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
	if cfg.Margin.CapacityRatio.IsPositive() {
		return applyRatio(cfg, s)
	}
	res := Result{Shortfall: s.Fusokugaku.GreaterThan(decimal.Zero)}
	capacity := s.Capacity()
	if res.Shortfall {
		// 追証が出ている日は建てない。Result.Shortfall も domain.MarginSummary.Fusokugaku も
		// そう書いてあるのに、実際は通知を出して通常どおり発注していた（2026-09-16 のレビュー）
		capacity = decimal.Zero
	}
	derivedLong, derivedShort := legTargets(cfg, capacity)

	res.Long = shrinkLong(&cfg, derivedLong)
	res.Short = shrinkShort(&cfg, derivedShort)
	res.Applied = res.Long.Changed() || res.Short.Changed()
	res.WatchOnly = cfg.Capital.Positions() == 0 && (cfg.Margin.Positions() == 0)
	return cfg, res
}

// ShockExceeds はショック日の倍率まで含めると、建玉の合計が保証金から導いた上限を超えるか。
// 超える額（倍率を掛けた合計）も返す。
//
// 倍率は 1 注文の予算に**縮小の後から**掛かる（execute/sizing.go の順序は検証と揃えてある）。
// つまりここで検算した額の shock_long_scale 倍が建ちうる。現行の設定（ショート 0.0）では
// 建可能額の 8 割ほどに収まるが、ショートを 0 より上げると枠を超えて発注が弾かれる
// ——気づけるように検算しておく（2026-09-16 のレビュー）。
func ShockExceeds(cfg config.Config, s Snapshot) (bool, decimal.Decimal) {
	capacity := s.Capacity()
	if !capacity.IsPositive() {
		return false, decimal.Zero
	}
	total := cfg.Capital.MaxCapital.Mul(cfg.Regime.ShockLongScale)
	if cfg.Margin.Enabled {
		total = total.Add(cfg.Margin.MaxCapital.Mul(cfg.Regime.ShockShortScale))
	}
	return total.GreaterThan(capacity), total
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
// 縮めた結果が 1 円未満に丸まっても 0 にはしない——0 は「上限なし」の意味なので、
// 縮めるほど栓が緩む向きになってしまう。1 円で止めて栓を締めたままにする。
func scaleCap(cap, ratio decimal.Decimal) decimal.Decimal {
	if cap.IsZero() {
		return cap
	}
	scaled := cap.Mul(ratio).Floor()
	if scaled.LessThanOrEqual(decimal.Zero) {
		return decimal.NewFromInt(1)
	}
	return scaled
}

// Describe は適用の結果を 1 行で。
func (r Result) Describe() string {
	if r.Ratio {
		return fmt.Sprintf("保証金の比で上限を決め直す: 長短合計 %s（ショック日 %s）→ ロング %s → %s / ショート %s → %s",
			r.NormalTotal.StringFixed(0), r.ShockTotal.StringFixed(0),
			r.Long.Before.StringFixed(0), r.Long.After.StringFixed(0),
			r.Short.Before.StringFixed(0), r.Short.After.StringFixed(0))
	}
	if !r.Applied {
		return "保証金による縮小なし（設定の水準で建てられる）"
	}
	return fmt.Sprintf("保証金で縮小: ロング %s → %s / ショート %s → %s",
		r.Long.Before.StringFixed(0), r.Long.After.StringFixed(0),
		r.Short.Before.StringFixed(0), r.Short.After.StringFixed(0))
}

// RatioTotals は margin.capacity_ratio のときの長短合計の上限（平日・ショック日）。
//
//	平日     = min(信用新規建可能額 × capacity_ratio,       capacity_ceiling)
//	ショック = min(信用新規建可能額 × shock_capacity_ratio, capacity_ceiling)
//
// 天井 0 は「天井なし」。建可能額が取れていなければ 0 を返す。
func RatioTotals(m config.Margin, s Snapshot) (normal, shock decimal.Decimal) {
	if !s.SinyouSinkidate.IsPositive() {
		return decimal.Zero, decimal.Zero
	}
	normal = s.SinyouSinkidate.Mul(m.CapacityRatio).Floor()
	shock = s.SinyouSinkidate.Mul(m.ShockCapacityRatio).Floor()
	if m.CapacityCeiling.IsPositive() {
		normal = decimal.Min(normal, m.CapacityCeiling)
		shock = decimal.Min(shock, m.CapacityCeiling)
	}
	return normal, shock
}

// applyRatio は margin.capacity_ratio の上限を**上げ下げ両方に**当てる（規則 R 専用。Validate が保証）。
//
// 長短合計 = RatioTotals の平日の値。ショートは設定の長短比で割った額を margin.max_capital で頭打ちにし
// （候補が少なく未検証の額に上げない）、残りをロングの capital.max_capital にする。ショートが使わなかった枠は
// いつもどおり spill_to_long でロングに回るので、長短合計は上限を超えない。
// ショック日のロングの総額は capital.ShockTotalCap（実行時の値）で頭打ちにする（execute.SizeDay）。
//
// 追証の日・建可能額が 0 以下の日は建てない（合計 0）。
func applyRatio(cfg config.Config, s Snapshot) (config.Config, Result) {
	res := Result{Shortfall: s.Fusokugaku.GreaterThan(decimal.Zero)}
	res.Long = LegChange{Before: cfg.Capital.MaxCapital, NBefore: cfg.Capital.Positions()}
	res.Short = LegChange{Before: cfg.Margin.MaxCapital, NBefore: cfg.Margin.Positions()}
	normal, shock := RatioTotals(cfg.Margin, s)
	// 追証の日と、当日ぶんのキャッシュで建可能額が 0 以下の日は建てない（WatchOnly → 通知）。
	// 保証金が取れない朝は呼ぶ側（applyMarginCap）がキャッシュ無し・古いとして設定の値に落としているので、
	// ここに来るのは「当日の値が 0 と読めた」朝だけ（2026-09-24 のレビュー。従来の Apply と同じ向き）
	if res.Shortfall || !normal.IsPositive() {
		normal, shock = decimal.Zero, decimal.Zero
	}
	// ショートの取り分は設定の長短比（legTargets と同じ）で割り、margin.max_capital を上限にする。
	// 先に満額を取ると、建可能額の小さい朝にロングが 0 になり、一時停止中は何も建たない
	_, shortShare := legTargets(cfg, normal)
	short := decimal.Min(cfg.Margin.MaxCapital, shortShare)
	if !cfg.Margin.Enabled {
		short = decimal.Zero
	}
	res.Short = shrinkShort(&cfg, short)
	long := normal
	if cfg.Margin.Enabled {
		long = normal.Sub(cfg.Margin.MaxCapital)
	}
	cfg.Capital.MaxCapital = long.Floor()
	if cfg.Capital.MaxCapital.IsNegative() {
		cfg.Capital.MaxCapital = decimal.Zero
	}
	cfg.Capital.ShockTotalCap = shock
	res.Long.After, res.Long.NAfter = cfg.Capital.MaxCapital, cfg.Capital.Positions()
	res.Applied = res.Long.Changed() || res.Short.Changed()
	res.WatchOnly = cfg.Capital.Positions() == 0 && cfg.Margin.Positions() == 0
	res.Ratio = true
	res.NormalTotal, res.ShockTotal = normal, shock
	return cfg, res
}
