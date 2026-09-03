package tactics

import (
	"fmt"
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
)

type Tactic interface {
	Name() string
	Describe() string
	WarmupBars() int
	Multipliers(bars []domain.Bar) []float64
}

// Constant は定額（ドル平均法）。常に 1.0 倍。
type Constant struct{}

func (c *Constant) Name() string {
	return "constant"
}

func (c *Constant) Describe() string {
	return "constant"
}

func (c *Constant) WarmupBars() int {
	return 1
}

func (c *Constant) Multipliers(bars []domain.Bar) []float64 {
	res := make([]float64, len(bars))
	for i := range res {
		res[i] = 1.0
	}
	return res
}

// BearStack は完全下降配列（終値 < MA20 < MA50 < MA200）で増額する。
type BearStack struct {
	Value float64
	Fast  int
	Mid   int
	Slow  int
}

func NewBearStack(multiplier float64, fast, mid, slow int) *BearStack {
	if multiplier <= 0 {
		multiplier = 4.0
	}
	if fast <= 0 {
		fast = 20
	}
	if mid <= 0 {
		mid = 50
	}
	if slow <= 0 {
		slow = 200
	}
	return &BearStack{
		Value: multiplier,
		Fast:  fast,
		Mid:   mid,
		Slow:  slow,
	}
}

func (b *BearStack) Name() string {
	return "bear_stack"
}

func (b *BearStack) Describe() string {
	return fmt.Sprintf("bear_stack(×%g, %d/%d/%d)", b.Value, b.Fast, b.Mid, b.Slow)
}

func (b *BearStack) WarmupBars() int {
	return b.Slow
}

func (b *BearStack) Multipliers(bars []domain.Bar) []float64 {
	n := len(bars)
	res := make([]float64, n)
	for i := range res {
		res[i] = 1.0
	}
	if n < b.Slow {
		return res
	}

	closes := make([]float64, n)
	for i, bar := range bars {
		c, _ := bar.Close.Float64()
		closes[i] = c
	}

	smaFast, _ := indicators.SMA(closes, b.Fast)
	smaMid, _ := indicators.SMA(closes, b.Mid)
	smaSlow, _ := indicators.SMA(closes, b.Slow)

	for i := b.Slow - 1; i < n; i++ {
		p := closes[i]
		f := smaFast[i]
		m := smaMid[i]
		s := smaSlow[i]

		if p < f && f < m && m < s {
			res[i] = b.Value
		}
	}

	return res
}

// StackLadder は弱気スコア（0〜6）に応じて段階的に増額する。
type StackLadder struct {
	Table map[int]float64
	Fast  int
	Mid   int
	Slow  int
}

func NewStackLadder(table map[int]float64, fast, mid, slow int) *StackLadder {
	if len(table) == 0 {
		table = map[int]float64{3: 1.5, 5: 2.0, 6: 4.0}
	}
	if fast <= 0 {
		fast = 20
	}
	if mid <= 0 {
		mid = 50
	}
	if slow <= 0 {
		slow = 200
	}
	return &StackLadder{
		Table: table,
		Fast:  fast,
		Mid:   mid,
		Slow:  slow,
	}
}

func (s *StackLadder) Name() string {
	return "stack_ladder"
}

func (s *StackLadder) Describe() string {
	return "stack_ladder"
}

func (s *StackLadder) WarmupBars() int {
	return s.Slow
}

func (s *StackLadder) Multipliers(bars []domain.Bar) []float64 {
	n := len(bars)
	res := make([]float64, n)
	for i := range res {
		res[i] = 1.0
	}
	if n < s.Slow {
		return res
	}

	closes := make([]float64, n)
	for i, bar := range bars {
		c, _ := bar.Close.Float64()
		closes[i] = c
	}

	smaFast, _ := indicators.SMA(closes, s.Fast)
	smaMid, _ := indicators.SMA(closes, s.Mid)
	smaSlow, _ := indicators.SMA(closes, s.Slow)

	// ソートされた閾値
	var thresholds []int
	for th := range s.Table {
		thresholds = append(thresholds, th)
	}
	sort.Ints(thresholds)

	for i := s.Slow - 1; i < n; i++ {
		p := closes[i]
		f := smaFast[i]
		m := smaMid[i]
		sl := smaSlow[i]

		score := 0
		if p < f {
			score++
		}
		if p < m {
			score++
		}
		if p < sl {
			score++
		}
		if f < m {
			score++
		}
		if f < sl {
			score++
		}
		if m < sl {
			score++
		}

		mult := 1.0
		for _, th := range thresholds {
			if score >= th {
				mult = s.Table[th]
			}
		}
		res[i] = mult
	}

	return res
}
