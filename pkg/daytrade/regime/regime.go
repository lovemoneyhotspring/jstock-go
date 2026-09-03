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
//     ギャップダウンが市場全体ではなく個別のニュースによるもので、逆張りが効かない
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
}

// Weak は「取引はするが資産曲線の合図で縮められた日」か（地合いが弱い日）。
func (v Verdict) Weak() bool { return v.Trade && v.Scale < 1.0 }

// Evaluate は設定で有効なゲートだけを見て、取引してよいか決める。
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

	if cfg.UsSkipHigh != nil && s.UsRet != nil &&
		floatOf(cfg.UsSkipLow) <= *s.UsRet && *s.UsRet < floatOf(*cfg.UsSkipHigh) &&
		(s.Vix == nil || decimal.NewFromFloat(*s.Vix).LessThanOrEqual(cfg.UsVixOverride)) {
		reason := fmt.Sprintf("前夜の S&P500 %+.2f%% が小幅高（%+.1f%%〜%+.1f%%）",
			*s.UsRet*100, floatOf(cfg.UsSkipLow)*100, floatOf(*cfg.UsSkipHigh)*100)
		if s.Vix != nil {
			reason += fmt.Sprintf("、VIX %.1f", *s.Vix)
		}
		reasons = append(reasons, reason)
	}

	notes := map[string]any{
		"month":         int(s.Day.Month()),
		"iv_prev":       floatPtr(s.IVPrev),
		"drift_bp":      scaledPtr(s.Drift, 1e4, 2),
		"market_gap_bp": scaledPtr(s.MarketGap, 1e4, 1),
		"recent_pnl":    floatPtr(s.RecentPnL),
		"us_ret_bp":     scaledPtr(s.UsRet, 1e4, 1),
		"vix":           floatPtr(s.Vix),
		"scale":         scale,
	}
	return Verdict{
		Trade:       len(reasons) == 0,
		Reasons:     reasons,
		Notes:       notes,
		Scale:       scale,
		ScaleReason: scaleReason,
	}
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
