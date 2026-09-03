package plan_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/shopspring/decimal"
)

var start = time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)

func TestBuildSaveLoadRoundTrip(t *testing.T) {
	days := fixture.BusinessDays(start, 40)
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "大型", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1000, Turnover: 5e8, MktCap: 9e11},
		{Code: "20000", Name: "中型", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 2000, Turnover: 4e8, MktCap: 5e11},
		{Code: "30000", Name: "小型", Market: "プライム", ProdCat: "011", Mrgn: "1",
			Base: 500, Turnover: 3e8, MktCap: 1e10},
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)

	day := days[len(days)-1]
	built, err := plan.Build(arch, cfg, day, calendar.FromArchive(arch), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if built.Meta.Day != day.Format(plan.DateLayout) {
		t.Errorf("判定日 = %s", built.Meta.Day)
	}
	if built.Meta.PrevDay != days[len(days)-2].Format(plan.DateLayout) {
		t.Errorf("前営業日 = %s", built.Meta.PrevDay)
	}
	if built.Meta.Candidates != 3 || built.Meta.Eligible != len(built.Eligible()) {
		t.Errorf("件数が合わない: candidates=%d eligible=%d/%d",
			built.Meta.Candidates, built.Meta.Eligible, len(built.Eligible()))
	}
	if built.Meta.Positions != 3 || built.Meta.CreatedAt == "" {
		t.Errorf("メタ情報が欠けている: %+v", built.Meta)
	}

	dir := t.TempDir()
	if _, _, err := plan.Save(built, dir); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := plan.Load(dir, day)
	if err != nil || !ok {
		t.Fatalf("読み戻せない: %v / %v", ok, err)
	}
	if len(loaded.Candidates) != len(built.Candidates) {
		t.Fatalf("候補 %d 件, want %d", len(loaded.Candidates), len(built.Candidates))
	}
	for i := range built.Candidates {
		a, b := built.Candidates[i], loaded.Candidates[i]
		if a.Code != b.Code || a.Symbol != b.Symbol || a.PrevClose != b.PrevClose ||
			a.Eligible != b.Eligible || a.ShortEligible != b.ShortEligible ||
			a.CapTercile != b.CapTercile || a.Shortable != b.Shortable {
			t.Errorf("往復で値が変わった:\n  %+v\n  %+v", a, b)
		}
	}
	if loaded.Meta.Positions != built.Meta.Positions {
		t.Errorf("メタ情報が往復で変わった: %+v", loaded.Meta)
	}
	if !loaded.Day().Equal(day) {
		t.Errorf("Day() = %v, want %v", loaded.Day(), day)
	}
}

func TestLoadMissingIsNotAnError(t *testing.T) {
	// plan が無い日は「無い」と答えるだけ（open が自分で作り直す）
	_, ok, err := plan.Load(t.TempDir(), start)
	if err != nil || ok {
		t.Errorf("Load = %v, %v; want false, nil", ok, err)
	}
}

func TestPrevCloseBySymbol(t *testing.T) {
	days := fixture.BusinessDays(start, 30)
	symbols := []fixture.Symbol{
		{Code: "10000", Name: "大型", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1234, Turnover: 5e8, MktCap: 9e11},
	}
	arch, err := fixture.Build(t.TempDir(), days, symbols)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(arch, config.Default(), days[len(days)-1], calendar.FromArchive(arch), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.PrevCloseBySymbol()["1000"]; got != 1234 {
		t.Errorf("前日終値 = %v, want 1234", got)
	}
	if syms := p.Symbols(p.Candidates); len(syms) != 1 || syms[0] != "1000" {
		t.Errorf("Symbols = %v", syms)
	}
}
