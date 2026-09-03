package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// RSIReversion は RSI 逆張り戦略。
//
// RSI が売られすぎ圏に沈んだら買い、買われすぎ圏に浮いたら売り。レンジ相場で
// 機能し、強いトレンドでは逆行し続けて損を膨らませる。そこで ADX でトレンドの
// 強さを測り、強トレンド時は発言を控える。順張り戦略との組み合わせが前提。
type RSIReversion struct {
	Period     int
	Oversold   float64
	Overbought float64
	ADXPeriod  int
	MaxADX     float64

	warmup int
}

// NewRSIReversion は RSI 逆張り戦略を作る。
func NewRSIReversion(period int, oversold, overbought float64, adxPeriod int, maxADX float64) (*RSIReversion, error) {
	if period <= 0 {
		period = 14
	}
	if adxPeriod <= 0 {
		adxPeriod = 14
	}
	if !(0 < oversold && oversold < overbought && overbought < 100) {
		return nil, fmt.Errorf("0 < oversold < overbought < 100 を満たすこと: %g, %g", oversold, overbought)
	}
	return &RSIReversion{
		Period:     period,
		Oversold:   oversold,
		Overbought: overbought,
		ADXPeriod:  adxPeriod,
		MaxADX:     maxADX,
		// ADX は内部で2段の Wilder 平滑化を行うため、その分の助走が要る
		warmup: maxInt(period, adxPeriod*3) + 1,
	}, nil
}

func (r *RSIReversion) Name() string    { return "rsi_reversion" }
func (r *RSIReversion) WarmupBars() int { return r.warmup }
func (r *RSIReversion) Describe() string {
	return fmt.Sprintf("%s(period=%d, oversold=%g, overbought=%g)", r.Name(), r.Period, r.Oversold, r.Overbought)
}

func (r *RSIReversion) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, r.warmup, r.evaluate), nil
}

func (r *RSIReversion) evaluate(symbol string, v View) *domain.Signal {
	rsiValue := last(v.RSI(r.Period))
	adxValue := last(v.ADX(r.ADXPeriod))
	if math.IsNaN(rsiValue) {
		return nil
	}

	// 強いトレンドが出ている間は逆張りしない。
	// 逆張り戦略が最も損をするのがこの局面なので、黙る判断を明示する。
	if !math.IsNaN(adxValue) && adxValue > r.MaxADX {
		return nil
	}

	meta := map[string]any{"rsi": rsiValue, "adx": adxValue}

	if rsiValue <= r.Oversold {
		depth := (r.Oversold - rsiValue) / r.Oversold
		return signal(r.Name(), symbol, 1.0, math.Min(1.0, 0.4+depth),
			fmt.Sprintf("RSI %.1f が売られすぎ圏（≦%g）", rsiValue, r.Oversold), meta)
	}
	if rsiValue >= r.Overbought {
		depth := (rsiValue - r.Overbought) / (100 - r.Overbought)
		return signal(r.Name(), symbol, -1.0, math.Min(1.0, 0.4+depth),
			fmt.Sprintf("RSI %.1f が買われすぎ圏（≧%g）", rsiValue, r.Overbought), meta)
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
