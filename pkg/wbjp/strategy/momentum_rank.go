package strategy

import (
	"fmt"
	"math"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

// MomentumRank は6ヶ月・12ヶ月のリターンを用いた中期モメンタム戦略。
type MomentumRank struct {
	TrendSMA   int // 200
	Ret6mDays  int // 126
	Skip1mDays int // 21
	Ret12mDays int // 252
}

func NewMomentumRank() *MomentumRank {
	return &MomentumRank{
		TrendSMA:   200,
		Ret6mDays:  126,
		Skip1mDays: 21,
		Ret12mDays: 252,
	}
}

func (s *MomentumRank) Name() string {
	return "momentum_rank"
}

func (s *MomentumRank) OnBars(symbol string, bars []domain.Bar) (*domain.Signal, error) {
	n := len(bars)
	if n < s.Ret12mDays+5 {
		return nil, nil
	}

	closes := make([]float64, n)
	for i, b := range bars {
		c, _ := b.Close.Float64()
		closes[i] = c
	}

	smaTrend, err := indicators.SMA(closes, s.TrendSMA)
	if err != nil {
		return nil, err
	}

	idx := n - 1
	p := closes[idx]
	tr := smaTrend[idx]

	// 1. 手仕舞い: 終値 < SMA200 (トレンド崩れ)
	if p < tr {
		return &domain.Signal{
			Strategy:   s.Name(),
			Symbol:     symbol,
			Direction:  -1.0,
			Confidence: 0.8,
			Reason:     fmt.Sprintf("終値 %.0f が SMA%d (%.0f) 割れ", p, s.TrendSMA, tr),
		}, nil
	}

	// 2. モメンタムの計算:
	// 直近1ヶ月（21日）を除いた過去6ヶ月リターン
	price1mAgo := closes[idx-s.Skip1mDays]
	price6mAgo := closes[idx-s.Ret6mDays]
	ret6m := (price1mAgo - price6mAgo) / price6mAgo

	// 過去12ヶ月リターン
	price12mAgo := closes[idx-s.Ret12mDays]
	ret12m := (p - price12mAgo) / price12mAgo

	// 6ヶ月、12ヶ月ともにプラスであること
	if ret6m <= 0 || ret12m <= 0 {
		return nil, nil
	}

	// 自信度: 6ヶ月リターン（上限 50% で 1.0 に正規化）
	conf := math.Min(1.0, math.Max(0.3, ret6m/0.5))

	return &domain.Signal{
		Strategy:   s.Name(),
		Symbol:     symbol,
		Direction:  1.0,
		Confidence: conf,
		Reason:     fmt.Sprintf("中期モメンタム (6ヶ月: %+.1f%%, 12ヶ月: %+.1f%%)", ret6m*100, ret12m*100),
	}, nil
}
