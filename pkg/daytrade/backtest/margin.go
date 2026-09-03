package backtest

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
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

	longRows := groupByDay(panel, func(r Row) bool {
		if !r.Eligible {
			return false
		}
		if cfg.Signal.SkipLimitDown && r.Open <= math.Max(r.LimitLow, 1.0) {
			return false
		}
		return true
	})
	longParams := legParams{
		n: nLong, budget: longBudget, capital: longCapital,
		weighting: cfg.Capital.Weighting, sign: 1,
		minGap: longMinGap, maxGap: longMaxGap,
		// 信用買い（日計り）なら手数料 0 円。金利・滑りは long_extra_cost_bp で見る
		commission: !cfg.Margin.LongViaMargin,
	}
	if cfg.Margin.LongViaMargin {
		longParams.extraCostBP = longExtra
	}
	longTrades := pickAndPrice(longRows, panel.Days, longParams)

	// ショートの母集団はロングと別（[margin] の segments / 除外。前夜の plan と同じ条件）
	shortRows := groupByDay(panel, func(r Row) bool {
		if !r.ShortEligible {
			return false
		}
		if cfg.Margin.SkipLimitUp && r.Open >= r.LimitHigh {
			return false
		}
		return true
	})
	shortTrades := pickAndPrice(shortRows, panel.Days, legParams{
		n: nShort, budget: shortBudget, capital: shortCapital,
		weighting: cfg.Margin.Weighting, sign: -1, descending: true,
		extraCostBP: shortExtra,
		commission:  false, // 立花証券の信用取引は手数料 0 円
		minGap:      shortMinGap, maxGap: shortMaxGap,
	})
	byKey := map[string]Row{}
	for _, r := range panel.Rows {
		byKey[r.Date.Format(dayLayout)+"|"+r.Code] = r
	}
	shortTrades = applyCarry(shortTrades, byKey, carryPenalty)

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
	longTrades = keepTrades(longTrades, longScale)
	shortTrades = keepTrades(shortTrades, shortMultiplier)

	return &MarginResult{
		Daily:        combined,
		LongTrades:   longTrades,
		ShortTrades:  shortTrades,
		Summary:      summarize(combined, longCapital+shortCapital, legAll),
		LongSummary:  summarize(combined, longCapital, legLong),
		ShortSummary: summarize(combined, shortCapital, legShort),
	}, nil
}

func keepTrades(trades []Trade, scale map[string]float64) []Trade {
	out := trades[:0]
	for _, t := range trades {
		if scale[t.Date.Format(dayLayout)] > 0 {
			out = append(out, t)
		}
	}
	return out
}

// applyRegimeSeesaw は日ごとに regime.Evaluate を呼び、ロングの資産曲線ゲートに応じて
// ショートの資金をシーソーさせる。
//
// 「戦略自身の直近の損益」（資産曲線ゲートの入力）は applyRegime と同じ定義——**ロング側**
// の実現損益のみを見る。ショート側の成績でロング側を動かすことはしない
// （ロングは既存 jp_gap_fade と同じ挙動を保つため）。
func applyRegimeSeesaw(longDaily, shortDaily map[string]*Daily, panel *Panel, cfg config.Config, signals *Inputs) []Daily {
	r := cfg.Regime
	m := cfg.Margin
	multiplierNormal, _ := m.MultiplierNormal.Float64()
	multiplierWeak, _ := m.MultiplierLongWeak.Float64()
	gaps := marketGapByDay(panel)

	out := make([]Daily, 0, len(panel.Days))
	longPnL := make([]float64, 0, len(panel.Days))
	longScales := make([]float64, 0, len(panel.Days))
	for i, day := range panel.Days {
		key := day.Format(dayLayout)
		long := longDaily[key]
		short := shortDaily[key]

		var recent *float64
		if r.EquityCurveDays > 0 && i >= r.EquityCurveDays {
			total := 0.0
			for j := i - r.EquityCurveDays; j < i; j++ {
				total += longPnL[j] * longScales[j]
			}
			recent = &total
		}
		verdict := regime.Evaluate(r, regime.Signals{
			Day:       day,
			IVPrev:    signals.lookup(signalsIV(signals), key),
			Drift:     signals.lookup(signalsDrift(signals), key),
			MarketGap: gaps[key],
			RecentPnL: recent,
			UsRet:     signals.lookup(signalsUsRet(signals), key),
			Vix:       signals.lookup(signalsVix(signals), key),
		})
		weak := verdict.Weak() // 資産曲線の合図（地合いが弱い）

		longScale := 0.0
		switch {
		case !verdict.Trade:
			longScale = 0
		case weak && !m.LongShrink:
			longScale = 1 // 合図はショートにだけ使い、ロングは縮めない
		default:
			longScale = verdict.Scale
		}
		shortMul := 0.0
		switch {
		case !verdict.Trade:
			shortMul = 0 // 危険信号そのものはショートも止める
		case weak:
			shortMul = multiplierWeak // シーソーで増強
		default:
			shortMul = multiplierNormal
		}
		longPnL = append(longPnL, long.PnL)
		longScales = append(longScales, longScale)

		d := Daily{
			Date:            day,
			LongScale:       longScale,
			ShortMultiplier: shortMul,
			LongPnL:         long.PnL * longScale,
			LongGross:       long.Gross * longScale,
			LongFees:        long.Fees * longScale,
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
		d.Amount = d.LongAmount + d.ShortAmount
		d.N = d.LongN + d.ShortN
		d.On = longScale > 0 || shortMul > 0
		d.Scale = longScale
		out = append(out, d)
	}
	return out
}

// RequiredMargin は長短を同時に最大で建てた日に要る委託保証金（立花証券は 33%）。
func RequiredMargin(cfg config.Config) (peak, required decimal.Decimal) {
	multiplier := cfg.Margin.MultiplierNormal
	if cfg.Margin.MultiplierLongWeak.GreaterThan(multiplier) {
		multiplier = cfg.Margin.MultiplierLongWeak
	}
	peak = cfg.Capital.MaxCapital.Add(cfg.Margin.MaxCapital.Mul(multiplier))
	return peak, peak.Mul(decimal.RequireFromString("0.33"))
}
