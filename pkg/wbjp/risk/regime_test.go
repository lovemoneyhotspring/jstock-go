package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

func regimeCfg() wbjpcfg.RegimeConfig {
	return wbjpcfg.RegimeConfig{
		Enabled:         true,
		Benchmark:       "1306",
		SMALong:         200,
		SMAMid:          50,
		SlopeLookback:   20,
		ExposureBull:    dec("1"),
		ExposureCaution: dec("0.5"),
		ExposureBear:    decimal.Zero,
	}
}

func TestRegimeExposure(t *testing.T) {
	cases := []struct {
		name         string
		close        string
		longMA       string
		midMA        string
		slope        string
		wantRegime   string
		wantExposure string
	}{
		{"強気: 長期線の上・中期線の上・上向き", "120", "100", "110", "5", RegimeBull, "1"},
		{"警戒: 中期線を割った", "105", "100", "110", "5", RegimeCaution, "0.5"},
		{"警戒: 長期線が下向き", "120", "100", "110", "0", RegimeCaution, "0.5"},
		{"弱気: 長期線を割った", "95", "100", "90", "5", RegimeBear, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			regime, exposure := RegimeExposure(regimeCfg(), RegimeInput{
				Close: decPtr(tc.close), LongMA: decPtr(tc.longMA),
				MidMA: decPtr(tc.midMA), Slope: decPtr(tc.slope),
			})
			if regime != tc.wantRegime || !exposure.Equal(dec(tc.wantExposure)) {
				t.Errorf("= %s / %s, want %s / %s", regime, exposure, tc.wantRegime, tc.wantExposure)
			}
		})
	}
}

func TestRegimeExposureDisabled(t *testing.T) {
	cfg := regimeCfg()
	cfg.Enabled = false
	regime, exposure := RegimeExposure(cfg, RegimeInput{})
	if regime != RegimeBull || !exposure.Equal(dec("1")) {
		t.Errorf("無効なら常に強気・1.0: %s / %s", regime, exposure)
	}
}

func TestRegimeExposureMissingDataIsBear(t *testing.T) {
	// 判断材料が無いときに全力で建てるのは、レジーム制御を入れた意図に反する。
	regime, exposure := RegimeExposure(regimeCfg(), RegimeInput{Close: decPtr("120")})
	if regime != RegimeBear || !exposure.IsZero() {
		t.Errorf("指標欠損は弱気扱い: %s / %s", regime, exposure)
	}
}

func TestApplyRegimeBearExitsAll(t *testing.T) {
	signals := map[string]domain.CombinedSignal{
		"7203": {Symbol: "7203", Direction: 0.9},
		"9984": {Symbol: "9984", Direction: 0.8},
	}
	positions := map[string]domain.Position{"7203": pos("7203", "100", "1000", "1000")}

	got, sizingEquity := ApplyRegime(RegimeBear, decimal.Zero, signals, positions, decimal.NewFromInt(1_000_000))
	if len(got) != 1 {
		t.Fatalf("保有銘柄だけが対象: %+v", got)
	}
	if got["7203"].Direction != -1.0 {
		t.Errorf("弱気は全手仕舞い: %+v", got["7203"])
	}
	if !sizingEquity.IsZero() {
		t.Errorf("サイジング総資産は 0: %s", sizingEquity)
	}
}

func TestApplyRegimeCautionBlocksNewEntries(t *testing.T) {
	signals := map[string]domain.CombinedSignal{
		"7203": {Symbol: "7203", Direction: 0.9},
		"9984": {Symbol: "9984", Direction: 0.8},
	}
	// 建玉 60 万 / 総資産 100 万 = 60% ≧ 露出上限 50% → 新規は止める
	positions := map[string]domain.Position{"7203": pos("7203", "100", "6000", "6000")}

	got, sizingEquity := ApplyRegime(RegimeCaution, dec("0.5"), signals, positions, decimal.NewFromInt(1_000_000))
	if _, ok := got["9984"]; ok {
		t.Error("露出上限に達しているのに新規シグナルが残っている")
	}
	if _, ok := got["7203"]; !ok {
		t.Error("保有中の銘柄は手仕舞い判断のため残すべき")
	}
	if !sizingEquity.Equal(decimal.NewFromInt(500_000)) {
		t.Errorf("サイジング総資産は 総資産 × 露出: %s", sizingEquity)
	}
}

func TestApplyRegimeCautionWithRoomKeepsSignals(t *testing.T) {
	signals := map[string]domain.CombinedSignal{"9984": {Symbol: "9984", Direction: 0.8}}
	got, _ := ApplyRegime(RegimeCaution, dec("0.5"), signals, nil, decimal.NewFromInt(1_000_000))
	if len(got) != 1 {
		t.Errorf("枠が残っていれば新規は通す: %+v", got)
	}
}

func TestApplyRegimeBullPassesThrough(t *testing.T) {
	signals := map[string]domain.CombinedSignal{"9984": {Symbol: "9984", Direction: 0.8}}
	got, sizingEquity := ApplyRegime(RegimeBull, dec("1"), signals, nil, decimal.NewFromInt(1_000_000))
	if len(got) != 1 || !sizingEquity.Equal(decimal.NewFromInt(1_000_000)) {
		t.Errorf("強気はそのまま: %+v / %s", got, sizingEquity)
	}
}
