package simulate

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

type AccumulationResult struct {
	StartDate          string
	EndDate            string
	Contributed        decimal.Decimal
	Units              float64
	AverageCost        float64
	TerminalValue      float64
	ControlAverageCost float64
	CostEdge           float64
	CapitalMultiple    float64
	TotalReturn        float64
	BoostedDays        int
}

// Simulate は日足と計画表から積立のバックテスト結果を計算する。
func Simulate(bars []domain.Bar, p *plan.AccumPlan, monthlyBudget decimal.Decimal) (*AccumulationResult, error) {
	if len(p.Rows) == 0 || len(bars) == 0 {
		return nil, fmt.Errorf("計画表または足データが空です")
	}

	n := len(bars)
	// 翌日の寄付（無ければ終値）を約定価格とする
	fillPrices := make([]float64, n)
	for i := 0; i < n; i++ {
		if i < n-1 {
			nextOpen, _ := bars[i+1].Open.Float64()
			if nextOpen > 0 {
				fillPrices[i] = nextOpen
			} else {
				nextClose, _ := bars[i+1].Close.Float64()
				fillPrices[i] = nextClose
			}
		} else {
			lastClose, _ := bars[i].Close.Float64()
			fillPrices[i] = lastClose
		}
	}

	totalContributed := decimal.Zero
	totalUnits := 0.0
	paydayCount := 0
	boostedDays := 0

	for i, r := range p.Rows {
		if r.Amount.GreaterThan(decimal.Zero) {
			amt, _ := r.Amount.Float64()
			fill := fillPrices[i]
			if fill > 0 {
				totalUnits += amt / fill
			}
			totalContributed = totalContributed.Add(r.Amount)
		}
		if r.Base.GreaterThan(decimal.Zero) {
			paydayCount++
		}
		if r.Extra.GreaterThan(decimal.Zero) {
			boostedDays++
		}
	}

	if totalUnits <= 0 || paydayCount == 0 {
		return nil, fmt.Errorf("一度も投下がありませんでした")
	}

	contribFloat, _ := totalContributed.Float64()
	avgCost := contribFloat / totalUnits

	lastClose, _ := bars[n-1].Close.Float64()
	termVal := totalUnits * lastClose

	// 対照群（同額を入金日に均等投資）
	perPayday := contribFloat / float64(paydayCount)
	controlUnits := 0.0
	for i, r := range p.Rows {
		if r.Base.GreaterThan(decimal.Zero) {
			fill := fillPrices[i]
			if fill > 0 {
				controlUnits += perPayday / fill
			}
		}
	}
	controlAvgCost := contribFloat / controlUnits
	costEdge := avgCost/controlAvgCost - 1.0

	mbFloat, _ := monthlyBudget.Float64()
	capitalMultiple := contribFloat / (mbFloat * float64(paydayCount))
	totReturn := termVal/contribFloat - 1.0

	return &AccumulationResult{
		StartDate:          p.Rows[0].Date,
		EndDate:            p.Rows[n-1].Date,
		Contributed:        totalContributed,
		Units:              totalUnits,
		AverageCost:        avgCost,
		TerminalValue:      termVal,
		ControlAverageCost: controlAvgCost,
		CostEdge:           costEdge,
		CapitalMultiple:    capitalMultiple,
		TotalReturn:        totReturn,
		BoostedDays:        boostedDays,
	}, nil
}
