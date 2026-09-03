package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// SMACross は移動平均クロス戦略（順張り）。
//
// 短期移動平均が長期移動平均を上抜けたら買い、下抜けたら売り。
// トレンドが続く相場で強く、横ばいでは往復ビンタを食らう。逆張り戦略と
// 組み合わせると相互に弱点を補える。
type SMACross struct {
	Fast int
	Slow int

	warmup int
}

// NewSMACross は移動平均クロス戦略を作る。
func NewSMACross(fast, slow int) (*SMACross, error) {
	if fast <= 0 {
		fast = 25
	}
	if slow <= 0 {
		slow = 75
	}
	if fast >= slow {
		return nil, fmt.Errorf("fast は slow より小さく: fast=%d, slow=%d", fast, slow)
	}
	// 長期線が確定した翌日から前日比較ができるので +1 本
	return &SMACross{Fast: fast, Slow: slow, warmup: slow + 1}, nil
}

func (s *SMACross) Name() string    { return "sma_cross" }
func (s *SMACross) WarmupBars() int { return s.warmup }
func (s *SMACross) Describe() string {
	return fmt.Sprintf("%s(fast=%d, slow=%d)", s.Name(), s.Fast, s.Slow)
}

func (s *SMACross) OnBars(ctx *Context) ([]domain.Signal, error) {
	return eachSymbol(ctx, s.warmup, s.evaluate), nil
}

func (s *SMACross) evaluate(symbol string, v View) *domain.Signal {
	fast := v.SMA(s.Fast)
	slow := v.SMA(s.Slow)

	currFast, currSlow := last(fast), last(slow)
	prevFast, prevSlow := ago(fast, 1), ago(slow, 1)
	if anyNaN(currFast, currSlow, prevFast, prevSlow) || currSlow == 0 {
		return nil
	}

	prevDiff := prevFast - prevSlow
	currDiff := currFast - currSlow

	// 乖離率を確信度に使う。離れているほどトレンドが明確とみなす。
	confidence := math.Min(1.0, math.Abs(currDiff)/currSlow*20.0)
	meta := map[string]any{"fast": currFast, "slow": currSlow}

	if prevDiff <= 0 && currDiff > 0 {
		return signal(s.Name(), symbol, 1.0, math.Max(confidence, 0.3),
			fmt.Sprintf("%d日線が%d日線を上抜け", s.Fast, s.Slow), meta)
	}
	if prevDiff >= 0 && currDiff < 0 {
		return signal(s.Name(), symbol, -1.0, math.Max(confidence, 0.3),
			fmt.Sprintf("%d日線が%d日線を下抜け", s.Fast, s.Slow), meta)
	}

	// クロスしていない間も、どちら側にいるかは意見として出す。
	// これが無いと、クロスした当日しか保有を維持できない。
	direction, side := -1.0, "下"
	if currDiff > 0 {
		direction, side = 1.0, "上"
	}
	return signal(s.Name(), symbol, direction, confidence*0.5,
		fmt.Sprintf("%d日線が%d日線の%sで推移", s.Fast, s.Slow, side), meta)
}
