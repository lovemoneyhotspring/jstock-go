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

// 空売り残高が重い銘柄はショートの母集団から外す（踏み上げの燃料）。
// 報告の無い銘柄（nil）は通す——報告義務は 0.5% 以上なので、無い＝軽い。
func TestShortEligibleCapsShortInterest(t *testing.T) {
	m := config.Margin{
		Enabled: true, Segments: []string{"prime"},
		MinTurnover:      decimal.NewFromInt(100_000_000),
		MaxShortInterest: decimal.NewFromFloat(0.02),
	}
	base := Candidate{Segment: "prime", TurnoverMed: 2e8, Shortable: true}
	light, heavy, edge := 0.01, 0.05, 0.02
	cases := []struct {
		name string
		si   *float64
		want bool
	}{
		{"報告なし", nil, true},
		{"軽い", &light, true},
		{"ちょうど上限", &edge, true},
		{"重い", &heavy, false},
	}
	for _, tc := range cases {
		c := base
		c.ShortInterest = tc.si
		if got := ShortEligible(c, m); got != tc.want {
			t.Errorf("%s: ShortEligible = %v, want %v", tc.name, got, tc.want)
		}
	}
	// 上限 0（既定）なら残高で落とさない
	m.MaxShortInterest = decimal.Zero
	c := base
	c.ShortInterest = &heavy
	if !ShortEligible(c, m) {
		t.Error("上限 0 なのに残高で落ちた")
	}
}
