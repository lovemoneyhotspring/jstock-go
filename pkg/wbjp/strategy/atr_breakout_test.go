package strategy

import (
	"fmt"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestATRBreakout_Breakout(t *testing.T) {
	strat := NewATRBreakout(5, 5, 0.005)

	var bars []domain.Bar
	// 5日間の保ち合い（高値105, 安値95, 終値100）
	for i := 1; i <= 10; i++ {
		dateStr := fmt.Sprintf("2026-08-%02d", i)
		bars = append(bars, domain.Bar{
			Date:   dateStr,
			Open:   decimal.NewFromInt(100),
			High:   decimal.NewFromInt(105),
			Low:    decimal.NewFromInt(95),
			Close:  decimal.NewFromInt(100),
			Volume: decimal.NewFromInt(1000),
		})
	}

	// 11日目: 高値110で上抜けブレイクアウト
	bars = append(bars, domain.Bar{
		Date:   "2026-08-11",
		Open:   decimal.NewFromInt(102),
		High:   decimal.NewFromInt(110),
		Low:    decimal.NewFromInt(100),
		Close:  decimal.NewFromInt(108),
		Volume: decimal.NewFromInt(2000),
	})

	sig, err := strat.OnBars("7203", bars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sig == nil {
		t.Fatalf("expected signal on breakout, got nil")
	}
	if sig.Direction != 1.0 {
		t.Errorf("direction = %g, want 1.0", sig.Direction)
	}
}
