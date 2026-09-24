package backtest

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// SimulateMargin はロング（jp_gap_fade と同じ規則）とショート（信用売り）を合わせて検証する。
//
// ショート側の資金配分は、ロング側の資産曲線ゲート（regime.equity_curve_days /
// equity_curve_scale）に連動する「シーソー」——ロング側が通常運転の日は
// margin.multiplier_normal 倍、縮小された日は margin.multiplier_long_weak 倍。
// 危険信号そのもの（月・IV 等）で止まる日は両側とも休む。
//
// 倍率によるショートの増減は、実際にその倍率で銘柄を選び直す（単元の切り捨てをやり直す）
// のではなく、基準資金で選んだ結果の損益を後から掛け増す近似——既存の equity_curve_scale
// によるロングの縮小と同じ手法。
func SimulateMargin(panel *Panel, cfg config.Config, signals *Inputs) (*MarginResult, error) {
	return SimulateMarginWith(panel, cfg, signals, Options{})
}

// SimulateMarginWith は約定モデルを指定して SimulateMargin を行う。
func SimulateMarginWith(panel *Panel, cfg config.Config, signals *Inputs, opts Options) (*MarginResult, error) {
	if !cfg.Margin.Enabled {
		return nil, fmt.Errorf("margin.enabled が false です（jp_gap_fade と同じ結果になるので Simulate を使う）")
	}
	nLong := cfg.Capital.Positions()
	nShort := cfg.Margin.Positions()
	if nLong == 0 {
		return nil, fmt.Errorf("capital.max_capital が 0 のため検証できません")
	}
	if nShort == 0 {
		return nil, fmt.Errorf("margin.max_capital が 0 のためショートを検証できません")
	}
	if err := requireSignals(cfg.Regime, signals); err != nil {
		return nil, err
	}

	longCapital, _ := cfg.Capital.MaxCapital.Float64()
	shortCapital, _ := cfg.Margin.MaxCapital.Float64()
	longExtra, _ := cfg.Margin.LongExtraCostBP.Float64()
	shortExtra, _ := cfg.Margin.ExtraCostBP.Float64()
	carryPenalty, _ := cfg.Margin.CarryPenalty.Float64()
	fill := opts.fill()
	byKey := rowsByKey(panel)

	longRows := groupByDay(panel, longKeep(cfg, opts))
	longParams := legParams{
		n: nLong, budget: cfg.Capital.BudgetPerOrder(), sign: 1, fill: fill,
		side: domain.SideBuy, signal: cfg.Signal, margin: cfg.Margin,
		pick: selection.TurnoverOptions(selection.PickOptions{
			Weighting:    cfg.Capital.Weighting,
			Side:         domain.SideBuy,
			MaxAmount:    cfg.Capital.MaxOrder,
			ValuePool:    cfg.Signal.ValuePool,
			MaxPerSector: cfg.Signal.MaxPerSector,
		}, cfg.Capital),
		// 信用買い（日計り）なら手数料 0 円。金利・滑りは long_extra_cost_bp で見る
		commission: !cfg.Margin.LongViaMargin,
	}
	if cfg.Margin.LongViaMargin {
		longParams.extraCostBP = longExtra
	}
	longParams = withDayRules(longParams, cfg, opts, signals, panel.Days)
	longTrades := pickAndPrice(longRows, panel.Days, longParams)
	longTrades = applyCarry(longTrades, byKey, 1, carryPenalty)

	// ショートの母集団はロングと別（[margin] の segments / 除外。前夜の plan と同じ条件）
	skipOpened := openedFilter(cfg, opts)
	outsideShortGap := outsideGap(cfg.Margin.MinGap, cfg.Margin.MaxGap)
	shortRows := groupByDay(panel, func(r Row) bool {
		return r.ShortEligible && !outsideShortGap(r) && !skipOpened(r)
	})
	shortTrades := pickAndPrice(shortRows, panel.Days, withDayRules(legParams{
		n: nShort, budget: cfg.Margin.BudgetPerOrder(), sign: -1,
		extraCostBP: shortExtra,
		commission:  false, // 立花証券の信用取引は手数料 0 円
		fill:        fill,
		side:        domain.SideSell, signal: cfg.Signal, margin: cfg.Margin,
		// 成行の新規売りは 50 単元まで（空売り価格規制）。PickFrom が同じ上限で切る。
		// パネルに売買単位は無いので 100 株単位とみなす
		pick: selection.PickOptions{
			Weighting: cfg.Margin.Weighting,
			Side:      domain.SideSell,
			MaxAmount: cfg.Margin.MaxOrder,
		},
	}, cfg, opts, signals, panel.Days))
	shortTrades = applyCarry(shortTrades, byKey, -1, carryPenalty)
	if cfg.Margin.Paused {
		shortTrades = nil // 一時停止: ショートは建てず、枠は SpillToLong でロングへ
	}

	// 規則 R は倍率を選ぶ前に掛けるので、spill を使わない設定でも日ごとに選ぶ経路を通す
	preScale := cfg.Capital.Weighting == config.WeightingTurnover
	if cfg.Margin.SpillToLong || preScale {
		return simulateMarginSpill(panel, cfg, signals, spillInputs{
			spill: cfg.Margin.SpillToLong, preScale: preScale,
			longParams: longParams, longRows: longRows, shortTrades: shortTrades, byKey: byKey,
			shortTotal: cfg.Margin.BudgetPerOrder().Mul(decimal.NewFromInt(int64(nShort))).InexactFloat64(), carryPenalty: carryPenalty,
			longCapital: longCapital, shortCapital: shortCapital,
			shockTotalCap: shockTotalCap(cfg),
		})
	}

	longDaily := dailyFromTrades(longTrades, panel.Days)
	shortDaily := dailyFromTrades(shortTrades, panel.Days)
	combined := applyRegimeSeesaw(longDaily, shortDaily, panel, cfg, signals)

	longScale := map[string]float64{}
	shortMultiplier := map[string]float64{}
	for _, d := range combined {
		key := d.Date.Format(dayLayout)
		longScale[key] = d.LongScale
		shortMultiplier[key] = d.ShortMultiplier
	}
	longTrades = scaleTrades(longTrades, longScale)
	shortTrades = scaleTrades(shortTrades, shortMultiplier)

	return &MarginResult{
		Daily:        combined,
		LongTrades:   longTrades,
		ShortTrades:  shortTrades,
		Summary:      summarize(combined, longCapital+shortCapital, legAll),
		LongSummary:  summarize(combined, longCapital, legLong),
		ShortSummary: summarize(combined, shortCapital, legShort),
	}, nil
}

// applyRegimeSeesaw は日ごとに regime.Evaluate を呼び、ロングの資産曲線ゲートに応じて
// ショートの資金をシーソーさせる。
//
// 「戦略自身の直近の損益」（資産曲線ゲートの入力）は applyRegime と同じ定義——**ロング側**
// の実現損益のみを見る。ショート側の成績でロング側を動かすことはしない
// （ロングは既存 jp_gap_fade と同じ挙動を保つため）。
func applyRegimeSeesaw(longDaily, shortDaily map[string]*Daily, panel *Panel, cfg config.Config, signals *Inputs) []Daily {
	gaps := marketGapByDay(panel)
	out := make([]Daily, 0, len(panel.Days))
	longPnL := make([]float64, 0, len(panel.Days))
	longScales := make([]float64, 0, len(panel.Days))
	longTraded := make([]bool, 0, len(panel.Days))
	for i, day := range panel.Days {
		key := day.Format(dayLayout)
		long := longDaily[key]
		short := shortDaily[key]
		recent := recentWindow(longPnL, longScales, longTraded, i, cfg.Regime.EquityCurveDays)
		verdict := evaluateDay(cfg, signals, day, gaps[key], recent)
		longScale, shortMul := seesawScales(verdict, cfg.Margin)
		longPnL = append(longPnL, ledgerPnL(long.PnL, long.Commission))
		longScales = append(longScales, longScale)
		longTraded = append(longTraded, longScale > 0 && long.N > 0)
		out = append(out, combineDay(day, long, short, longScale, longScale, shortMul))
	}
	return out
}

// evaluateDay は日次の危険信号の判定（検証用の材料の引き方をまとめたもの）。
func evaluateDay(cfg config.Config, signals *Inputs, day time.Time, marketGap *float64, recent *float64) regime.Verdict {
	key := day.Format(dayLayout)
	return regime.Evaluate(cfg.Regime, regime.Signals{
		Day:       day,
		IVPrev:    signals.lookup(signalsIV(signals), key),
		Drift:     signals.lookup(signalsDrift(signals), key),
		MarketGap: marketGap,
		RecentPnL: recent,
		UsRet:     signals.lookup(signalsUsRet(signals), key),
		Vix:       signals.lookup(signalsVix(signals), key),
	})
}

// seesawScales は判定からロングの倍率とショートの倍率を出す（実運用の open と同じ順序:
// 止める → 縮小／シーソー → ショック）。
func seesawScales(verdict regime.Verdict, m config.Margin) (longScale, shortMul float64) {
	multiplierNormal, _ := m.MultiplierNormal.Float64()
	multiplierWeak, _ := m.MultiplierLongWeak.Float64()
	weak := verdict.Weak() // 資産曲線の合図（地合いが弱い）
	switch {
	case !verdict.Trade:
		longScale = 0
	case weak && !m.LongShrink:
		longScale = 1 // 合図はショートにだけ使い、ロングは縮めない
	default:
		longScale = verdict.Scale
	}
	switch {
	case !verdict.Trade, verdict.ShortOff && !m.Paused:
		shortMul = 0 // 危険信号そのものはショートも止める（ShortOff はショートだけ。一時停止中は枠をロングへ回すので残す）
	case weak:
		shortMul = multiplierWeak // シーソーで増強
	default:
		shortMul = multiplierNormal
	}
	// ショック日は脚ごとの倍率を掛ける
	if verdict.Trade {
		longScale *= verdict.ShockLong
		shortMul *= verdict.ShockShort
	}
	return longScale, shortMul
}

// combineDay は両脚の日次を倍率で畳んで 1 日にする。
// longMul はロングの損益に掛ける倍率。選ぶ前に倍率を掛け終えた日（規則 R）は 1、
// それ以外は longScale と同じ。表示の LongScale / Scale は常に longScale。
func combineDay(day time.Time, long, short *Daily, longScale, longMul, shortMul float64) Daily {
	d := Daily{
		Date:            day,
		LongScale:       longScale,
		ShortMultiplier: shortMul,
		LongPnL:         long.PnL * longMul,
		LongGross:       long.Gross * longMul,
		LongFees:        long.Fees * longMul,
		LongCommission:  long.Commission * longMul,
		LongAmount:      long.Amount * longMul,
		ShortPnL:        short.PnL * shortMul,
		ShortGross:      short.Gross * shortMul,
		ShortFees:       short.Fees * shortMul,
		ShortAmount:     short.Amount * shortMul,
	}
	if longScale > 0 {
		d.LongN = long.N
	}
	if shortMul > 0 {
		d.ShortN = short.N
	}
	d.PnL = d.LongPnL + d.ShortPnL
	d.Gross = d.LongGross + d.ShortGross
	d.Fees = d.LongFees + d.ShortFees
	d.Commission = d.LongCommission
	d.Amount = d.LongAmount + d.ShortAmount
	d.N = d.LongN + d.ShortN
	d.On = longScale > 0 || shortMul > 0
	d.Scale = longScale
	return d
}

// spillInputs は simulateMarginSpill の材料。
type spillInputs struct {
	// spill はショートの余りをロングへ回すか（margin.spill_to_long）。
	spill bool
	// preScale はロングの倍率（縮小・ショック日）を選ぶ前の予算に掛けるか（規則 R）。
	preScale    bool
	longParams  legParams
	longRows    map[string][]Row
	shortTrades []Trade
	byKey       map[string]Row
	// shortTotal はショートの 1 日の総予算（1 注文の予算 × N、倍率 1 のとき）。
	shortTotal   float64
	carryPenalty float64
	longCapital  float64
	shortCapital float64
	// shockTotalCap はショック日のロングの総額の上限（0 なら上限なし。shockTotalCap）。
	shockTotalCap decimal.Decimal
}

// preScaledBudget は規則 R の選ぶ前の 1 注文の予算: 倍率を掛け、ショック日は総額を limit で頭打ち
// （本番の execute.SizeDay と同じ順序: 倍率の後、余りの前）。limit 0 は上限なし。
func preScaledBudget(budget decimal.Decimal, longScale float64, shock bool, n int, limit decimal.Decimal) decimal.Decimal {
	b := budget.Mul(decimal.NewFromFloat(longScale)).Floor()
	if shock && limit.IsPositive() && n > 0 {
		if b.Mul(decimal.NewFromInt(int64(n))).GreaterThan(limit) {
			b = limit.Div(decimal.NewFromInt(int64(n))).Floor()
		}
	}
	return b
}

// shockTotalCap はショック日のロングの総額の上限。本番は margincap が朝の建可能額 × shock_capacity_ratio
// を capital.ShockTotalCap に入れ（execute.SizeDay が頭打ち）、保証金が読めない朝は長短の固定合計
// （capital.max_capital + margin.max_capital）にする（cmd/daytrade の ratioFallbackConfig）。
// 検証は資金を固定値で回すので、後者と同じ値で頭打ちにする。比を置かない設定は上限なし（本番と同じ）。
func shockTotalCap(cfg config.Config) decimal.Decimal {
	limit := cfg.Capital.ShockTotalCap
	if !cfg.Margin.CapacityRatio.IsPositive() {
		return limit
	}
	fixed := cfg.Capital.MaxCapital
	if cfg.Margin.Enabled {
		fixed = fixed.Add(cfg.Margin.MaxCapital)
	}
	if !limit.IsPositive() || limit.GreaterThan(fixed) {
		limit = fixed
	}
	return limit
}

// simulateMarginSpill は margin.spill_to_long の検証。ショートで使わなかった資金をその日の
// ロングに回すので、日ごとに「ショートの使用額 → ロングの総予算 → ロングの選定」の順に決める
// （ロングを先に固定して後から倍率を掛ける applyRegimeSeesaw とは順序が違う）。
//
// 回す金額 = ショートの倍率 × (総予算 − 使用額)。ショック日など倍率 0 の日は回さない
// （ロングは ×1.5 で 450 万になり、そこへ 200 万を足すと保証金の枠を超えるため）。
// ロングの銘柄数は 総予算 ÷ 1 注文の予算（capital.max_positions が上限）。
func simulateMarginSpill(panel *Panel, cfg config.Config, signals *Inputs, in spillInputs) (*MarginResult, error) {
	shortUsed := map[string]float64{}
	for _, t := range in.shortTrades {
		shortUsed[t.Date.Format(dayLayout)] += t.Amount
	}
	shortDaily := dailyFromTrades(in.shortTrades, panel.Days)
	gaps := marketGapByDay(panel)
	nLong := in.longParams.n
	budget := in.longParams.budget
	maxN := cfg.Capital.MaxPositions

	var longTrades []Trade
	out := make([]Daily, 0, len(panel.Days))
	longPnL := make([]float64, 0, len(panel.Days))
	longScales := make([]float64, 0, len(panel.Days))
	longTraded := make([]bool, 0, len(panel.Days))
	shortMultiplier := map[string]float64{}
	for i, day := range panel.Days {
		key := day.Format(dayLayout)
		recent := recentWindow(longPnL, longScales, longTraded, i, cfg.Regime.EquityCurveDays)
		verdict := evaluateDay(cfg, signals, day, gaps[key], recent)
		longScale, shortMul := seesawScales(verdict, cfg.Margin)
		shortMultiplier[key] = shortMul

		spill := 0.0
		if in.spill && shortMul > 0 && in.shortTotal > shortUsed[key] {
			spill = shortMul * (in.shortTotal - shortUsed[key])
		}
		// 規則 R（turnover）は本番（execute.SizeDay）と同じく、倍率を**選ぶ前の予算**に掛ける。
		// 1 銘柄の上限は売買代金で頭打ちになるので、選んだ後に株数を ×1.5 すると上限を破り、
		// 余りが下の順位へ回らない（本番と銘柄数も金額も変わる）。等金額は後から掛けても同じなので従来どおり
		pickBudget, longMul := budget, longScale
		if in.preScale {
			pickBudget, longMul = preScaledBudget(budget, longScale, verdict.Shock, nLong, in.shockTotalCap), 1
			if longScale <= 0 {
				longMul = 0
			}
		}
		// 銘柄数と 1 注文の予算は本番と同じ式（selection.SpillInto）。
		n, dayBudget := selection.SpillInto(nLong, pickBudget, budget, decimal.NewFromFloat(spill), maxN)
		var dayTrades []Trade
		if longScale > 0 {
			dayTrades = pickDay(in.longRows[key], in.longParams, n, dayBudget)
			dayTrades = applyCarry(dayTrades, in.byKey, 1, in.carryPenalty)
		}
		long := sumDaily(day, dayTrades)

		// 資産曲線ゲートの入力は「倍率 1 のときの損益 × 倍率」。前もって掛けた日は倍率 1 として積む
		longPnL = append(longPnL, ledgerPnL(long.PnL, long.Commission))
		longScales = append(longScales, longMul)
		longTraded = append(longTraded, longScale > 0 && long.N > 0)
		out = append(out, combineDay(day, long, shortDaily[key], longScale, longMul, shortMul))
		longTrades = append(longTrades, markScale(scaleTrades(dayTrades, map[string]float64{key: longMul}), longScale)...)
	}
	shortTrades := scaleTrades(in.shortTrades, shortMultiplier)
	return &MarginResult{
		Daily:        out,
		LongTrades:   longTrades,
		ShortTrades:  shortTrades,
		Summary:      summarize(out, in.longCapital+in.shortCapital, legAll),
		LongSummary:  summarize(out, in.longCapital, legLong),
		ShortSummary: summarize(out, in.shortCapital, legShort),
	}, nil
}

// sumDaily は 1 日ぶんの取引を畳む。
func sumDaily(day time.Time, trades []Trade) *Daily {
	d := &Daily{Date: day, Scale: 1, On: true}
	for _, t := range trades {
		d.PnL += t.PnL
		d.Gross += t.Gross
		d.Fees += t.Fees
		d.Commission += t.Commission
		d.Amount += t.Amount
		d.N++
	}
	return d
}

// RequiredMargin は長短を同時に最大で建てた日に要る委託保証金（立花証券は 33%）。
// ショック日の倍率（regime.shock_*_scale）で建玉が増えるなら、その日のほうを取る。
func RequiredMargin(cfg config.Config) (peak, required decimal.Decimal) {
	multiplier := cfg.Margin.MultiplierNormal
	if cfg.Margin.MultiplierLongWeak.GreaterThan(multiplier) {
		multiplier = cfg.Margin.MultiplierLongWeak
	}
	peak = cfg.Capital.MaxCapital.Add(cfg.Margin.MaxCapital.Mul(multiplier))
	shock := cfg.Capital.MaxCapital.Mul(cfg.Regime.ShockLongScale).
		Add(cfg.Margin.MaxCapital.Mul(multiplier).Mul(cfg.Regime.ShockShortScale))
	if shock.GreaterThan(peak) {
		peak = shock
	}
	return peak, peak.Mul(decimal.RequireFromString("0.33"))
}
