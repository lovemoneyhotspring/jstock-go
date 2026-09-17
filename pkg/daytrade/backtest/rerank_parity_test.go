package backtest_test

import (
	"math"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 並べ替えの機械学習の特徴量が、前夜の plan（universe.Build）とパネルで同じ値になるか。
// ずれると本番と検証で並びが変わる。
func TestPlanAndPanelRerankFeaturesMatch(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	day, prevDay := days[len(days)-1], days[len(days)-2]
	cfg := parityConfig()
	panel, err := backtest.LoadPanelWith(arch, day, day, cfg, backtest.PanelOptions{KeepAll: true})
	if err != nil {
		t.Fatal(err)
	}
	cands, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]universe.Candidate{}
	for _, c := range cands {
		byCode[c.Code] = c
	}
	compared, moving := 0, 0
	for _, r := range panel.Rows {
		c, ok := byCode[r.Code]
		if !ok {
			continue
		}
		for _, f := range []struct {
			name        string
			plan, panel *float64
		}{
			{"ret1", c.Ret1, r.Ret1}, {"ret5", c.Ret5, r.Ret5}, {"ret20", c.Ret20, r.Ret20},
			{"pos20", c.Pos20, r.Pos20}, {"prev_intraday", c.PrevIntraday, r.PrevIntraday},
		} {
			if (f.plan == nil) != (f.panel == nil) {
				t.Errorf("%s の %s: plan %v / パネル %v", r.Code, f.name, f.plan, f.panel)
				continue
			}
			if f.plan == nil {
				continue
			}
			if math.Abs(*f.plan-*f.panel) > 1e-12 {
				t.Errorf("%s の %s: plan %v / パネル %v", r.Code, f.name, *f.plan, *f.panel)
			}
			if *f.plan != 0 {
				moving++
			}
			compared++
		}
	}
	if compared == 0 || moving == 0 {
		t.Fatalf("突き合わせた値が無い（%d 件、非ゼロ %d 件）——テストが何も確かめていない", compared, moving)
	}
}

// rank_by = lgbm でも本番と検証が同じ銘柄・株数を選ぶか（TestPlanAndBacktestPickTheSame のロング版）。
func TestPlanAndBacktestPickTheSameWithRerank(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	day, prevDay := days[len(days)-1], days[len(days)-2]
	cfg := parityConfig()
	cfg.Signal.RankBy = config.RankByLGBM
	cfg.Signal.Model = repoModel(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	panel, err := backtest.LoadPanel(arch, day, day, cfg)
	if err != nil {
		t.Fatal(err)
	}
	quotes := map[string]selection.Quote{}
	for _, r := range panel.Rows {
		quotes[r.Code] = selection.Quote{Symbol: r.Code, Price: decimal.NewFromFloat(r.Open),
			PrevClose: decimal.NewFromFloat(r.PrevClose)}
	}
	cands, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cands {
		cands[i].Symbol = cands[i].Code
	}
	ranked := selection.Rank(keepLong(cands), quotes, cfg.Signal)
	if len(ranked) == 0 || ranked[0].Score == nil {
		t.Fatal("機械学習で並べていない")
	}
	picks := selection.PickFrom(ranked, selection.PickOptions{
		N: cfg.Capital.Positions(), Budget: cfg.Capital.BudgetPerOrder(),
		Weighting: cfg.Capital.Weighting, Side: domain.SideBuy,
		MaxAmount: cfg.Capital.MaxOrder,
		ValuePool: cfg.Signal.ValuePool, MaxPerSector: cfg.Signal.MaxPerSector,
	})
	result, err := backtest.SimulateMargin(panel, cfg, &backtest.Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(picks) == 0 || len(result.LongTrades) != len(picks) {
		t.Fatalf("建てた銘柄数 検証 %d / 本番 %d", len(result.LongTrades), len(picks))
	}
	want := map[string]float64{}
	for _, p := range picks {
		want[p.Code] = p.Quantity.InexactFloat64()
	}
	for _, tr := range result.LongTrades {
		if q, ok := want[tr.Code]; !ok || q != tr.Shares {
			t.Errorf("%s: 検証 %v 株 / 本番 %v 株（本番が選んだか %v）", tr.Code, tr.Shares, q, ok)
		}
	}
}

func repoModel(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "daytrade", "models", "lgbm_rank.txt")
}
