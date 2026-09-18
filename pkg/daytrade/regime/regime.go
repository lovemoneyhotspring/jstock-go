// Package regime は危険信号（局面のゲート）。毎日計算して記録し、設定で有効にしたものだけが
// 取引を止める。
//
// 2018・2021 年の負けは「市場が寄り高・引け安を 1 年続けた」ことによる（研究ノート）。
// 前日までに観測できる信号は次の 5 つ。効き方が検証で違うので、**個別に有効化**する。
//
//   - 月（skip_months）: 12 月は 9 年中 7 年がマイナス。IS/OOS ともに改善。既定で有効
//   - 市場の日中ドリフト（drift_gate）: TOPIX の寄り→引けの 20 日平均。IS では 2018・2021 を
//     黒字にするが OOS では利益を 3 割削る。既定は無効（診断値として記録）
//   - 資産曲線（equity_curve_days / equity_curve_scale）: 戦略自身の直近 20 日の実現損益が
//     0 以下なら資金を半分にする。休むのではなく縮める（MaxDD −50→−30 万、利益 −2%）
//   - IV（iv_gate）: 日経 225 オプションの前日 IV
//   - 前夜の米国（us_skip_high）: S&P500 が小幅高（0〜+1%）で VIX が低い翌日は、東証の
//     ギャップダウンが市場全体ではなく個別のニュースによるもので、ギャップの深さで並べる逆張りが
//     効かない。us_skip_legs = "short" ならショートだけ休む（LightGBM で並べるロングはこの日も稼げる）
//
// 市場のギャップ（|中央値ギャップ| > 1% の日）は例外で、ドリフトが負でも取引する。
// 急落・急騰の寄付は逆張りが最も効く日で、ゲートで外すと損をする。
package regime

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

// Signals は判定日の朝に分かる値。無いものは nil（そのゲートは効かせない）。
type Signals struct {
	Day time.Time
	// IVPrev は前日の日経 225 オプション IV（BaseVol 中央値）。
	IVPrev *float64
	// Drift は TOPIX の寄り→引けリターンの直近 N 日平均（前日まで）。
	Drift *float64
	// MarketGap は 9:00 の市場ギャップ。候補全体の中央値ギャップで代用する
	// （TOPIX の寄付は取れない）。
	MarketGap *float64
	// RecentPnL は戦略自身の直近 N 日の損益（円、前日まで）。
	RecentPnL *float64
	// UsRet は前夜の S&P500 の終値リターン。
	UsRet *float64
	// Vix は前夜の VIX 終値。
	Vix *float64
}

// Verdict は判定の結果。
type Verdict struct {
	Trade bool
	// Reasons は止めた理由（複数）。取引するときは空。
	Reasons []string
	// Notes は記録用の診断値。
	Notes map[string]any
	// Scale は資金に掛ける倍率（1 = そのまま）。資産曲線で縮めるときに 1 未満。
	Scale float64
	// ScaleReason は縮めた理由（縮めていなければ空）。
	ScaleReason string
	// Shock は予期せぬ急落の日（shock_market_gap / shock_us_ret のどちらかに掛かった）。
	// ShockLong / ShockShort はその日にロング／ショートの資金へ掛ける倍率（ショックでなければ 1）。
	// ゲートで止めた日（Trade が偽）には意味を持たない。
	Shock       bool
	ShockReason string
	ShockLong   float64
	ShockShort  float64
	// ShortOff はロングは取引するがショートだけ建てない日か（us_skip_legs = "short" の米国のゲート）。
	// ShortOffReason はその理由。Trade が偽の日には意味を持たない。
	ShortOff       bool
	ShortOffReason string
	// UsLow は前夜の米国市場が小幅高の帯に入る日か。どの脚を休むか（us_skip_legs）とは別で、
	// **Trade が偽の日にも立つ**——並べ方をこの日だけ替える設定（signal.rank_by_us_low）が使う。
	UsLow bool
}

// Weak は「取引はするが資産曲線の合図で縮められた日」か（地合いが弱い日）。
func (v Verdict) Weak() bool { return v.Trade && v.Scale < 1.0 }

// Evaluate は設定で有効なゲートだけを見て、取引してよいか決める。
// IsUsLow は前夜の米国市場が「小幅高の帯」（us_skip_low 〜 us_skip_high）に入る日か。
// VIX が us_vix_override を超える日は帯の中でも小幅高とみなさない（急落の前触れは別扱い）。
// どの脚を休むか（us_skip_legs）とは切り離してあり、並べ方をこの日だけ替える設定
// （signal.rank_by_us_low）も同じ判定を使う——二重に書くと片方だけ直して食い違うため。
func IsUsLow(cfg config.Regime, usRet, vix *float64) bool {
	return cfg.UsSkipHigh != nil && usRet != nil &&
		floatOf(cfg.UsSkipLow) <= *usRet && *usRet < floatOf(*cfg.UsSkipHigh) &&
		(vix == nil || decimal.NewFromFloat(*vix).LessThanOrEqual(cfg.UsVixOverride))
}

func Evaluate(cfg config.Regime, s Signals) Verdict {
	var reasons []string

	if slices.Contains(cfg.SkipMonths, int(s.Day.Month())) {
		reasons = append(reasons, fmt.Sprintf("%d 月は休む", int(s.Day.Month())))
	}
	if cfg.IVGate.GreaterThan(decimal.Zero) && s.IVPrev != nil &&
		decimal.NewFromFloat(*s.IVPrev).LessThanOrEqual(cfg.IVGate) {
		reasons = append(reasons, fmt.Sprintf("IV %.1f ≤ %s", *s.IVPrev, cfg.IVGate.String()))
	}
	// 大きなギャップの日はドリフトのゲートを無視する（急落の寄付が最も効く）
	bigGap := s.MarketGap != nil && math.Abs(*s.MarketGap) > floatOf(cfg.DriftGapOverride)
	if cfg.DriftGate != nil && s.Drift != nil && !bigGap &&
		decimal.NewFromFloat(*s.Drift).LessThanOrEqual(*cfg.DriftGate) {
		reasons = append(reasons, fmt.Sprintf("市場の日中ドリフト %+.1f bp ≤ %+.0f bp",
			*s.Drift*1e4, floatOf(*cfg.DriftGate)*10_000))
	}

	scale := 1.0
	scaleReason := ""
	if cfg.EquityCurveDays > 0 && s.RecentPnL != nil && *s.RecentPnL <= 0 {
		text := fmt.Sprintf("直近 %d 日の損益 %s 円 ≤ 0", cfg.EquityCurveDays, comma(*s.RecentPnL))
		if cfg.EquityCurveScale.LessThanOrEqual(decimal.Zero) {
			reasons = append(reasons, text)
		} else {
			scale = floatOf(cfg.EquityCurveScale)
			scaleReason = fmt.Sprintf("%s → 資金を %g 倍に縮小", text, scale)
		}
	}

	shortOff, shortOffReason := false, ""
	usLow := IsUsLow(cfg, s.UsRet, s.Vix)
	if usLow {
		reason := fmt.Sprintf("前夜の S&P500 %+.2f%% が小幅高（%+.1f%%〜%+.1f%%）",
			*s.UsRet*100, floatOf(cfg.UsSkipLow)*100, floatOf(*cfg.UsSkipHigh)*100)
		if s.Vix != nil {
			reason += fmt.Sprintf("、VIX %.1f", *s.Vix)
		}
		if cfg.UsSkipLegs == config.UsSkipLegsShort {
			shortOff, shortOffReason = true, reason+" → ショートだけ休む"
		} else {
			reasons = append(reasons, reason)
		}
	}

	// ショック日: 止めるのではなく、脚ごとの倍率を変える（既定は 1 = 記録だけ）。
	// 急落の寄付は逆張りが最も効く日で、ロングを増やしショートを減らすのが検証の結論
	shock := false
	var shockWhy []string
	if cfg.ShockMarketGap != nil && s.MarketGap != nil && *s.MarketGap <= floatOf(*cfg.ShockMarketGap) {
		shock = true
		shockWhy = append(shockWhy, fmt.Sprintf("市場ギャップ %+.2f%% ≤ %+.1f%%", *s.MarketGap*100, floatOf(*cfg.ShockMarketGap)*100))
	}
	if cfg.ShockUsRet != nil && s.UsRet != nil && *s.UsRet <= floatOf(*cfg.ShockUsRet) {
		shock = true
		shockWhy = append(shockWhy, fmt.Sprintf("前夜の S&P500 %+.2f%% ≤ %+.1f%%", *s.UsRet*100, floatOf(*cfg.ShockUsRet)*100))
	}
	shockLong, shockShort := 1.0, 1.0
	shockReason := ""
	if shock {
		shockLong, shockShort = floatOf(cfg.ShockLongScale), floatOf(cfg.ShockShortScale)
		shockReason = fmt.Sprintf("ショック日（%s）→ ロング ×%g、ショート ×%g", joinReasons(shockWhy), shockLong, shockShort)
	}

	if len(reasons) > 0 {
		// 止めた日はショートも建たないので、ショートだけの休みは取引する日にだけ立てる。
		// 診断値の short_off も Verdict と揃える（notes を作る前に消す）
		shortOff, shortOffReason = false, ""
	}

	notes := map[string]any{
		"month":         int(s.Day.Month()),
		"shock":         shock,
		"iv_prev":       floatPtr(s.IVPrev),
		"drift_bp":      scaledPtr(s.Drift, 1e4, 2),
		"market_gap_bp": scaledPtr(s.MarketGap, 1e4, 1),
		"recent_pnl":    floatPtr(s.RecentPnL),
		"us_ret_bp":     scaledPtr(s.UsRet, 1e4, 1),
		"vix":           floatPtr(s.Vix),
		"scale":         scale,
		"short_off":     shortOff,
	}
	return Verdict{
		Trade:          len(reasons) == 0,
		Reasons:        reasons,
		Notes:          notes,
		Scale:          scale,
		ScaleReason:    scaleReason,
		Shock:          shock,
		ShockReason:    shockReason,
		ShockLong:      shockLong,
		ShockShort:     shockShort,
		ShortOff:       shortOff,
		ShortOffReason: shortOffReason,
		UsLow:          usLow,
	}
}

func joinReasons(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "、"
		}
		out += p
	}
	return out
}

// MarketGapOf は候補全体のギャップの中央値（9:00 の市場ギャップの代用）。
func MarketGapOf(gaps []float64) *float64 {
	if len(gaps) == 0 {
		return nil
	}
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	n := len(sorted)
	var v float64
	if n%2 == 1 {
		v = sorted[n/2]
	} else {
		v = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return &v
}

func floatOf(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

func floatPtr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func scaledPtr(v *float64, factor float64, digits int) any {
	if v == nil {
		return nil
	}
	shift := math.Pow(10, float64(digits))
	return math.Round(*v*factor*shift) / shift
}

// comma は円の桁区切り（ログと画面で同じ見た目にする）。
func comma(v float64) string {
	return decimal.NewFromFloat(v).Round(0).String()
}
