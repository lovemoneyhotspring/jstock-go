package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// ATRBreakout は ATR フィルタ付きドンチャン・ブレイクアウト戦略。
//
// 過去 N 日の高値を上抜けたら買い、安値を下抜けたら売り。ボラティリティが
// 極端に小さい（＝だましになりやすい）局面は見送る。大きな相場の初動を
// 捉えるが、勝率は低く、少数の大きな勝ちで稼ぐ。
type ATRBreakout struct {
	Channel     int
	ATRPeriod   int
	MinATRRatio float64

	warmup int
}

// NewATRBreakout はブレイクアウト戦略を作る。
//
// minATRRatio は ATR/終値 の下限。これ未満なら値動きが小さすぎるとみなして
// 見送る（だましを減らすためのフィルタ）。
func NewATRBreakout(channel, atrPeriod int, minATRRatio float64) (*ATRBreakout, error) {
	if channel <= 0 {
		channel = 20
	}
	if atrPeriod <= 0 {
		atrPeriod = 14
	}
	if minATRRatio < 0 {
		return nil, fmt.Errorf("min_atr_ratio は 0 以上: %g", minATRRatio)
	}
	return &ATRBreakout{
		Channel:     channel,
		ATRPeriod:   atrPeriod,
		MinATRRatio: minATRRatio,
		warmup:      maxInt(channel, atrPeriod) + 2,
	}, nil
}

func (a *ATRBreakout) Name() string    { return "atr_breakout" }
func (a *ATRBreakout) WarmupBars() int { return a.warmup }
func (a *ATRBreakout) Describe() string {
	return fmt.Sprintf("%s(channel=%d, atr_period=%d)", a.Name(), a.Channel, a.ATRPeriod)
}

func (a *ATRBreakout) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, a.warmup, a.evaluate), nil
}

func (a *ATRBreakout) evaluate(symbol string, v View) *domain.Signal {
	upper := last(v.DonchianHigh(a.Channel))
	lower := last(v.DonchianLow(a.Channel))
	atrValue := last(v.ATR(a.ATRPeriod))
	closePrice := last(v.Closes())

	if anyNaN(upper, lower, atrValue, closePrice) || closePrice <= 0 {
		return nil
	}

	atrRatio := atrValue / closePrice
	if atrRatio < a.MinATRRatio {
		return nil // 値動きが乏しく、だましになりやすい
	}

	// ボラティリティが大きいほどブレイクの信頼度を上げる（上限あり）
	confidence := math.Min(1.0, 0.4+atrRatio*20.0)
	meta := map[string]any{"upper": upper, "lower": lower, "atr": atrValue, "atr_ratio": atrRatio}

	if last(v.Highs()) > upper {
		return signal(a.Name(), symbol, 1.0, confidence,
			fmt.Sprintf("%d日高値 %.1f を上抜け", a.Channel, upper), meta)
	}
	if last(v.Lows()) < lower {
		return signal(a.Name(), symbol, -1.0, confidence,
			fmt.Sprintf("%d日安値 %.1f を下抜け", a.Channel, lower), meta)
	}
	return nil
}
