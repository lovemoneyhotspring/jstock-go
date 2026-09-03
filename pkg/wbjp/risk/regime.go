package risk

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// レジームの名前。
const (
	RegimeBull    = "bull"
	RegimeCaution = "caution"
	RegimeBear    = "bear"
)

// RegimeInput はレジーム判定に使う指数の 1 日ぶんの値。
//
// 指標が欠けている（ウォームアップ中など）ときは nil を渡す。
type RegimeInput struct {
	// Close は指数の終値。
	Close *decimal.Decimal
	// LongMA は長期移動平均（sma_long）。
	LongMA *decimal.Decimal
	// MidMA は中期移動平均（sma_mid）。
	MidMA *decimal.Decimal
	// Slope は長期線の傾き（長期線 − slope_lookback 日前の長期線）。
	Slope *decimal.Decimal
}

// RegimeExposure はその足の相場レジームと露出上限を返す。無効なら常に強気・1.0。
//
// 指数の足が無い（ウォームアップ中）ときは判断できないので **弱気扱い** に
// する。判断材料が無いときに全力で建てるのは、レジーム制御を入れた意図に
// 反する。
func RegimeExposure(cfg wbjpcfg.RegimeConfig, in RegimeInput) (string, decimal.Decimal) {
	if !cfg.Enabled {
		return RegimeBull, decimal.NewFromInt(1)
	}
	if in.Close == nil || in.LongMA == nil || in.MidMA == nil || in.Slope == nil {
		return RegimeBear, cfg.ExposureBear
	}
	if in.Close.LessThan(*in.LongMA) {
		return RegimeBear, cfg.ExposureBear
	}
	if in.Close.LessThan(*in.MidMA) || in.Slope.LessThanOrEqual(decimal.Zero) {
		return RegimeCaution, cfg.ExposureCaution
	}
	return RegimeBull, cfg.ExposureBull
}

// ApplyRegime はレジームに応じてシグナルを絞り、サイジングに渡す総資産を返す。
//
//   - 露出 0（弱気）: 保有銘柄をすべて手仕舞いのシグナルに置き換え、新規は禁止
//   - 露出 1 未満（警戒）: 建玉比率が既に上限に達していれば新規を止め、
//     保有中の銘柄のシグナルだけ残す（手仕舞い判断は続ける）
//   - 露出 1（強気）: そのまま
//
// 副作用を持たない純粋関数。呼び出し（engine）側は返り値を使うだけでよい。
func ApplyRegime(
	regime string,
	exposure decimal.Decimal,
	signals map[string]domain.CombinedSignal,
	positions map[string]domain.Position,
	equity decimal.Decimal,
) (map[string]domain.CombinedSignal, decimal.Decimal) {
	held := make(map[string]struct{})
	gross := decimal.Zero
	for sym, pos := range positions {
		if pos.Quantity.GreaterThan(decimal.Zero) {
			held[sym] = struct{}{}
		}
		gross = gross.Add(pos.MarketValue())
	}

	one := decimal.NewFromInt(1)
	result := signals

	switch {
	case exposure.LessThanOrEqual(decimal.Zero):
		// 弱気は全手仕舞い。戦略が何を言っていても現金へ退避する。
		result = make(map[string]domain.CombinedSignal, len(held))
		for sym := range held {
			result[sym] = domain.CombinedSignal{
				Symbol:        sym,
				Direction:     -1.0,
				Contributions: map[string]float64{},
				Reason:        fmt.Sprintf("レジーム %s: 全手仕舞い", regime),
			}
		}
	case exposure.LessThan(one):
		if equity.GreaterThan(decimal.Zero) && gross.Div(equity).GreaterThanOrEqual(exposure) {
			// 露出が既に上限。保有中の銘柄だけ残して新規を止める
			// （手仕舞いシグナルは通す必要があるので消さない）。
			result = make(map[string]domain.CombinedSignal, len(held))
			for sym, sig := range signals {
				if _, ok := held[sym]; ok {
					result[sym] = sig
				}
			}
		}
	}

	return result, equity.Mul(exposure)
}
