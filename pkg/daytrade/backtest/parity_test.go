package backtest_test

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 本番（前夜の plan → 9:00 の open）と検証（backtest）が同じ日に同じ銘柄・同じ株数を
// 選ぶことを押さえる。
//
// 段 1〜2 で選定（selection）も母集団（universe）も 1 つの関数に寄せたので、
// これは**構造的に通る**はずのテスト。落ちるとしたら universe.Build の SQL と
// パネルの SQL で特徴量（売買代金の窓・20 日ボラ・時価総額の分位の母数）がずれたとき
// ——そこが検証と実運用のずれの入口になる。
func TestPlanAndBacktestPickTheSame(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	day := days[len(days)-1]
	prevDay := days[len(days)-2]

	for _, tc := range []struct {
		name   string
		short  bool
		prefer bool
	}{{"ロング", false, false}, {"ロング（選定の優先あり）", false, true}, {"ショート", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parityConfig()
			if tc.prefer {
				// 合成の足は最後の 10 日しか動かずストキャス RSI が出ないので、値の出る RSI(2) で経路を通す
				cfg.Signal.Prefer = config.Prefer{Indicator: config.PreferRSI2, Max: 50, Pool: 20}
			}
			// 検証は 1 日だけ回す（その日の選定を突き合わせる）
			panel, err := backtest.LoadPanel(arch, day, day, cfg)
			if err != nil {
				t.Fatal(err)
			}
			quotes := map[string]selection.Quote{}
			for _, r := range panel.Rows {
				quotes[r.Code] = selection.Quote{
					Symbol:    r.Code,
					Price:     decimal.NewFromFloat(r.Open),
					PrevClose: decimal.NewFromFloat(r.PrevClose),
				}
			}
			// 本番の経路: 前夜の plan（universe.Build）→ 9:00 の順位付けと株数
			cands, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
			if err != nil {
				t.Fatal(err)
			}
			for i := range cands {
				cands[i].Symbol = cands[i].Code // 検証は 5 桁コードのまま気配を引く
			}
			var picks []selection.Pick
			if tc.short {
				picks = selection.PickFrom(selection.RankShort(keepShort(cands), quotes, cfg.Margin),
					selection.PickOptions{
						N: cfg.Margin.Positions(), Budget: cfg.Margin.BudgetPerOrder(),
						Weighting: cfg.Margin.Weighting, Side: domain.SideSell,
						MaxAmount: cfg.Margin.MaxOrder,
					})
			} else {
				picks = selection.PickFrom(selection.Rank(keepLong(cands), quotes, cfg.Signal),
					selection.PickOptions{
						N: cfg.Capital.Positions(), Budget: cfg.Capital.BudgetPerOrder(),
						Weighting: cfg.Capital.Weighting, Side: domain.SideBuy,
						MaxAmount: cfg.Capital.MaxOrder,
						ValuePool: cfg.Signal.ValuePool, MaxPerSector: cfg.Signal.MaxPerSector,
					})
			}

			// 検証の経路
			result, err := backtest.SimulateMargin(panel, cfg, &backtest.Inputs{})
			if err != nil {
				t.Fatal(err)
			}
			trades := result.LongTrades
			if tc.short {
				trades = result.ShortTrades
			}

			if len(picks) == 0 {
				t.Fatal("本番の経路が 1 銘柄も選んでいない（テストが何も確かめていない）")
			}
			if len(trades) != len(picks) {
				t.Fatalf("建てた銘柄数 検証 %d / 本番 %d", len(trades), len(picks))
			}
			byCode := map[string]selection.Pick{}
			for _, p := range picks {
				byCode[p.Code] = p
			}
			for _, tr := range trades {
				p, ok := byCode[tr.Code]
				if !ok {
					t.Errorf("検証だけが建てた銘柄: %s", tr.Code)
					continue
				}
				if got, want := tr.Shares, p.Quantity.InexactFloat64(); got != want {
					t.Errorf("%s の株数 検証 %v / 本番 %v", tr.Code, got, want)
				}
				if got, want := tr.Amount, p.Amount().InexactFloat64(); got != want {
					t.Errorf("%s の金額 検証 %v / 本番 %v", tr.Code, got, want)
				}
			}
		})
	}
}

func parityConfig() config.Config {
	cfg := baseConfig()
	cfg.Margin.Enabled = true
	cfg.Margin.MaxCapital = decimal.NewFromInt(2_000_000)
	cfg.Margin.OrderBudget = decimal.NewFromInt(670_000)
	cfg.Margin.Weighting = "equal"
	cfg.Margin.SpillToLong = false // 突き合わせるのは選定なので、日ごとの予算は動かさない
	cfg.Margin.MultiplierNormal = decimal.NewFromInt(1)
	cfg.Margin.MultiplierLongWeak = decimal.NewFromInt(1)
	return cfg
}

func keepLong(cands []universe.Candidate) []universe.Candidate {
	out := cands[:0:0]
	for _, c := range cands {
		if c.Eligible {
			out = append(out, c)
		}
	}
	return out
}

func keepShort(cands []universe.Candidate) []universe.Candidate {
	out := cands[:0:0]
	for _, c := range cands {
		if c.ShortEligible {
			out = append(out, c)
		}
	}
	return out
}
