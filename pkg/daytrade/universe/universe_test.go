package universe

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/shopspring/decimal"
)

func TestEligibleExcludesLoss(t *testing.T) {
	cfg := config.Universe{
		Segments: []string{"prime"}, MinTurnover: decimal.NewFromInt(100_000_000),
		ExcludeCapTerciles: 1,
	}
	c := Candidate{Segment: "prime", TurnoverMed: 2e8, CapTercile: 3, Loss: true}
	if !Eligible(c, cfg) {
		t.Fatal("exclude_loss が偽なら赤字でも母集団に入る")
	}
	cfg.ExcludeLoss = true
	if Eligible(c, cfg) {
		t.Error("exclude_loss が真なら赤字は外れる")
	}
	// 本決算が見つからない銘柄（Loss = false）は落とさない
	c.Loss = false
	if !Eligible(c, cfg) {
		t.Error("赤字でない銘柄は残る")
	}
}
