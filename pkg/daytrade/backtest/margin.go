package backtest

import (
	"fmt"
	"math"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
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
	longBudget, _ := cfg.Capital.BudgetPerOrder().Float64()
	shortBudget, _ := cfg.Margin.BudgetPerOrder().Float64()
	longExtra, _ := cfg.Margin.LongExtraCostBP.Float64()
	shortExtra, _ := cfg.Margin.ExtraCostBP.Float64()
	carryPenalty, _ := cfg.Margin.CarryPenalty.Float64()
	longMinGap, _ := cfg.Signal.MinGap.Float64()
	longMaxGap, _ := cfg.Signal.MaxGap.Float64()
	shortMinGap, _ := cfg.Margin.MinGap.Float64()
	shortMaxGap, _ := cfg.Margin.MaxGap.Float64()
	shortMaxOrder, _ := cfg.Margin.MaxOrder.Float64()
	longMaxOrder, _ := cfg.Capital.MaxOrder.Float64()
	fill := opts.fill()
	byKey := rowsByKey(panel)

	longRows := groupByDay(panel, longKeep(cfg, opts))
	longParams := legParams{
		n: nLong, budget: longBudget,
		weighting: cfg.Capital.Weighting, sign: 1,
		minGap: longMinGap, maxGap: longMaxGap, fill: fill,
		rankBy: cfg.Signal.RankBy, maxAmount: longMaxOrder,
		// 信用買い（日計り）なら手数料 0 円。金利・滑りは long_extra_cost_bp で見る
		commission: !cfg.Margin.LongViaMargin,
	}
	if cfg.Margin.LongViaMargin {
		longParams.extraCostBP = longExtra
	}
	longTrades := pickAndPrice(longRows, panel.Days, longParams)
	longTrades = applyCarry(longTrades, byKey, 1, carryPenalty)

	// ショートの母集団はロングと別（[margin] の segments / 除外。前夜の plan と同じ条件）
	skipOpened := openedFilter(cfg, opts)
	shortRows := groupByDay(panel, func(r Row) bool {
		if !r.ShortEligible {
			return false
		}
		if cfg.Margin.SkipLimitUp && r.Open >= r.LimitHigh {
			return false
		}
		return !skipOpened(r)
	})
	shortTrades := pickAndPrice(shortRows, panel.Days, legParams{
		n: nShort, budget: shortBudget,
		weighting: cfg.Margin.Weighting, sign: -1, descending: true,
		extraCostBP: shortExtra,
		commission:  false, // 立花証券の信用取引は手数料 0 円
		minGap:      shortMinGap, maxGap: shortMaxGap,
		// 成行の新規売りは 50 単元まで（空売り価格規制）。実運用の selection も同じ上限で切る
		maxShares: float64(broker.ShortSaleMarketLimit),
		fill:      fill,
		maxAmount: shortMaxOrder,
	})
	shortTrades = applyCarry(shortTrades, byKey, -1, carryPenalty)

	if cfg.Margin.SpillToLong {
		return simulateMarginSpill(panel, cfg, signals, spillInputs{
			longParams: longParams, longRows: longRows, shortTrades: shortTrades, byKey: byKey,
			shortTotal: shortBudget * float64(nShort), carryPenalty: carryPenalty,
			longCapital: longCapital, shortCapital: shortCapital,
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
		out = append(out, combineDay(day, long, short, longScale, shortMul))
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
	case !verdict.Trade:
		shortMul = 0 // 危険信号そのものはショートも止める
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
func combineDay(day time.Time, long, short *Daily, longScale, shortMul float64) Daily {
	d := Daily{
		Date:            day,
		LongScale:       longScale,
		ShortMultiplier: shortMul,
		LongPnL:         long.PnL * longScale,
		LongGross:       long.Gross * longScale,
		LongFees:        long.Fees * longScale,
		LongCommission:  long.Commission * longScale,
		LongAmount:      long.Amount * longScale,
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
	longParams  legParams
	longRows    map[string][]Row
	shortTrades []Trade
	byKey       map[string]Row
	// shortTotal はショートの 1 日の総予算（1 注文の予算 × N、倍率 1 のとき）。
	shortTotal   float64
	carryPenalty float64
	longCapital  float64
	shortCapital float64
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
		if shortMul > 0 && in.shortTotal > shortUsed[key] {
			spill = shortMul * (in.shortTotal - shortUsed[key])
		}
		total := budget*float64(nLong) + spill
		n := nLong
		if spill > 0 {
			n = int(math.Floor(total / budget))
			if n < nLong {
				n = nLong
			}
			if maxN > 0 && n > maxN {
				n = maxN
			}
		}
		dayTrades := pickDay(in.longRows[key], in.longParams, n, total)
		dayTrades = applyCarry(dayTrades, in.byKey, 1, in.carryPenalty)
		long := sumDaily(day, dayTrades)

		longPnL = append(longPnL, ledgerPnL(long.PnL, long.Commission))
		longScales = append(longScales, longScale)
		longTraded = append(longTraded, longScale > 0 && long.N > 0)
		out = append(out, combineDay(day, long, shortDaily[key], longScale, shortMul))
		longTrades = append(longTrades, scaleTrades(dayTrades, map[string]float64{key: longScale})...)
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
