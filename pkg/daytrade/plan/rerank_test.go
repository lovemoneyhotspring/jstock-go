package plan_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/parquet-go/parquet-go"
)

// 並べ替えの特徴量と益回りは plan を保存して読み戻しても残る（open は plan しか見ない）。
func TestSaveLoadKeepsRerankFeatures(t *testing.T) {
	day, _ := time.Parse(plan.DateLayout, "2026-09-18")
	ey, r1, r5, r20, pos, pi := 0.05, -0.01, 0.02, -0.08, 0.3, 0.004
	p := plan.Plan{
		Meta: plan.Meta{Day: "2026-09-18", PrevDay: "2026-09-17", RerankFeatures: plan.RerankFeaturesVersion},
		Candidates: []universe.Candidate{
			{Code: "10000", Symbol: "1000", PrevClose: 1000, Eligible: true,
				EarnYield: &ey, Ret1: &r1, Ret5: &r5, Ret20: &r20, Pos20: &pos, PrevIntraday: &pi},
			{Code: "20000", Symbol: "2000", PrevClose: 500, Eligible: true},
		},
	}
	dir := t.TempDir()
	if _, _, err := plan.Save(p, dir); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := plan.Load(dir, day)
	if err != nil || !ok {
		t.Fatalf("読み戻せない: %v / %v", ok, err)
	}
	if loaded.Meta.RerankFeatures != plan.RerankFeaturesVersion {
		t.Errorf("特徴量の版 = %d", loaded.Meta.RerankFeatures)
	}
	c := loaded.Candidates[0]
	for name, pair := range map[string][2]*float64{
		"earn_yield": {c.EarnYield, &ey}, "ret1": {c.Ret1, &r1}, "ret5": {c.Ret5, &r5},
		"ret20": {c.Ret20, &r20}, "pos20": {c.Pos20, &pos}, "prev_intraday": {c.PrevIntraday, &pi},
	} {
		if pair[0] == nil || *pair[0] != *pair[1] {
			t.Errorf("%s が往復しない: %v", name, pair[0])
		}
	}
	if d := loaded.Candidates[1]; d.Ret1 != nil || d.EarnYield != nil {
		t.Errorf("値の無い銘柄が nil でない: %v %v", d.Ret1, d.EarnYield)
	}
	sig := config.Signal{RankBy: config.RankByLGBM}
	if got := loaded.Signal(sig).RankBy; got != config.RankByLGBM {
		t.Errorf("特徴量のある plan で rank_by = %s", got)
	}
}

// 特徴量の列が無い古い plan も読め、機械学習では並べない（既存規則に戻す）。
func TestOldPlanFallsBackToRule(t *testing.T) {
	day, _ := time.Parse(plan.DateLayout, "2026-09-17")
	dir := t.TempDir()
	p := plan.Plan{
		Meta:       plan.Meta{Day: "2026-09-17", PrevDay: "2026-09-16"},
		Candidates: []universe.Candidate{{Code: "10000", Symbol: "1000", PrevClose: 1000, Eligible: true}},
	}
	parquetPath, _, err := plan.Save(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	// 旧版の列だけで書き直す
	type oldRecord struct {
		Code      string  `parquet:"Code"`
		Symbol    string  `parquet:"symbol"`
		PrevClose float64 `parquet:"prev_close"`
		Eligible  bool    `parquet:"eligible"`
	}
	if err := parquet.WriteFile(parquetPath, []oldRecord{{Code: "10000", Symbol: "1000", PrevClose: 1000, Eligible: true}}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := plan.Load(dir, day)
	if err != nil || !ok {
		t.Fatalf("古い plan を読めない: %v / %v", ok, err)
	}
	if c := loaded.Candidates[0]; c.Ret1 != nil || c.EarnYield != nil || !c.Eligible {
		t.Errorf("古い plan の読み方が違う: %+v", c)
	}
	sig := config.Signal{RankBy: config.RankByLGBM, Model: "x"}
	if got := loaded.Signal(sig); got.RankBy != config.RankByGapVol {
		t.Errorf("古い plan で rank_by = %s, want %s", got.RankBy, config.RankByGapVol)
	}
	if got := loaded.Signal(config.Signal{RankBy: config.RankByGap}); got.RankBy != config.RankByGap {
		t.Errorf("lgbm 以外を書き換えた: %s", got.RankBy)
	}
}
