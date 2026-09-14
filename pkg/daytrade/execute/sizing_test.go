package execute

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func yenOf(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

// sizingConfig はロング 300 万 N3・ショート 300 万 N3（どちらも 1 注文 100 万）。
func sizingConfig(margin, spill bool) config.Config {
	cfg := config.Default()
	cfg.Capital.MaxCapital, cfg.Capital.OrderBudget, cfg.Capital.MaxPositions = yenOf(3_000_000), yenOf(1_000_000), 10
	cfg.Margin.Enabled = margin
	cfg.Margin.MaxCapital, cfg.Margin.OrderBudget = yenOf(3_000_000), yenOf(1_000_000)
	cfg.Margin.MultiplierNormal, cfg.Margin.MultiplierLongWeak = decimal.NewFromInt(1), decimal.NewFromInt(1)
	cfg.Margin.SpillToLong = spill
	return cfg
}

func tradeDay() regime.Verdict {
	return regime.Verdict{Trade: true, Scale: 1, ShockLong: 1, ShockShort: 1}
}

func shortPick(amount int64) selection.Pick {
	return selection.Pick{Symbol: "S", Side: domain.SideSell, Price: yenOf(amount / 100), Quantity: yenOf(100)}
}

func TestSizeDay(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config.Config
		verdict    regime.Verdict
		placed     Placed
		tiedLong   int64
		watchOnly  bool
		shortPicks []selection.Pick
		wantLong   Sizing
		wantShort  *Sizing
		wantSpill  int64
		wantDone   bool
	}{
		{name: "1 回目", cfg: sizingConfig(false, false), verdict: tradeDay(),
			wantLong: Sizing{N: 3, Budget: yenOf(1_000_000)}},
		{name: "再実行で N を建て終えている", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 3, LongAmount: yenOf(3_000_000)},
			wantLong: Sizing{N: 0, Budget: yenOf(1_000_000)}, wantDone: true},
		{name: "再実行で残り 2 件", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 1, LongAmount: yenOf(1_000_000)},
			wantLong: Sizing{N: 2, Budget: yenOf(1_000_000)}},
		// 候補が 2 銘柄しか無く 1 銘柄 150 万で建てた回の再実行。件数では 1 件残るが資金は使い切っている
		{name: "再実行で件数は残るが資金を使い切っている", cfg: sizingConfig(false, false), verdict: tradeDay(),
			placed:   Placed{Long: 2, LongAmount: yenOf(3_000_000)},
			wantLong: Sizing{N: 0, Budget: yenOf(1_000_000)}},
		{name: "様子見", cfg: func() config.Config { c := sizingConfig(true, true); c.Capital.MaxCapital = decimal.Zero; return c }(),
			verdict: tradeDay(), watchOnly: true,
			wantLong: Sizing{N: watchRowsForTest, Budget: yenOf(1_000_000), Weighting: "equal"}},
		{name: "弱い日は縮める", cfg: sizingConfig(false, false),
			verdict:  regime.Verdict{Trade: true, Scale: 0.5, ScaleReason: "損益 → 縮小", ShockLong: 1, ShockShort: 1},
			wantLong: Sizing{N: 3, Budget: yenOf(500_000)}},
		{name: "持ち越しの拘束", cfg: sizingConfig(false, false), verdict: tradeDay(), tiedLong: 1_500_000,
			wantLong: Sizing{N: 1, Budget: yenOf(1_000_000)}},
		// 1 回目: ショートが 100 万しか使わなければ余り 200 万で N=5（selection.SpillInto と同じ）
		{name: "余りをロングへ（1 回目）", cfg: sizingConfig(true, true), verdict: tradeDay(),
			shortPicks: []selection.Pick{shortPick(1_000_000)},
			wantLong:   Sizing{N: 5, Budget: yenOf(1_000_000)}, wantShort: &Sizing{N: 3, Budget: yenOf(1_000_000)},
			wantSpill: 2_000_000},
		// 前の回が余りで 4 件建て、N（3）を超えている。1 件は通らなかった → 残り 1 件
		{name: "前の回の余りが N を超えた再実行", cfg: sizingConfig(true, true), verdict: tradeDay(),
			placed:    Placed{Long: 4, LongAmount: yenOf(4_000_000), Short: 3, ShortAmount: yenOf(1_000_000)},
			wantLong:  Sizing{N: 1, Budget: yenOf(1_000_000)},
			wantSpill: 2_000_000},
		// 前の回: ショート 1 件（150 万）+ ロング 2 件（225 万）で締め切り。再実行でショートの候補なし。
		// 余りは 300 − 150 = 150 万（「残り 2 件 × 100 万 = 200 万」を数え直さない）→ 1 日 4 件 × 112.5 万、
		// 残り 2 件。長短の合計がショートの枠を超えない
		{name: "余りの再実行", cfg: sizingConfig(true, true), verdict: tradeDay(),
			placed:    Placed{Long: 2, LongAmount: yenOf(2_250_000), Short: 1, ShortAmount: yenOf(1_500_000)},
			wantLong:  Sizing{N: 2, Budget: yenOf(1_125_000)},
			wantShort: &Sizing{N: 1, Budget: yenOf(1_000_000)},
			wantSpill: 1_500_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := SizingInput{Cfg: c.cfg, Verdict: c.verdict, Placed: c.placed, TiedLong: yenOf(c.tiedLong),
				WatchOnly: c.watchOnly, WatchRows: watchRowsForTest}
			d := SizeDay(in)
			long, spill, _ := d.WithSpill(c.shortPicks)
			wantWeighting := c.wantLong.Weighting
			if wantWeighting == "" {
				wantWeighting = c.cfg.Capital.Weighting
			}
			if long.N != c.wantLong.N || !long.Budget.Equal(c.wantLong.Budget) || long.Weighting != wantWeighting {
				t.Errorf("ロング = %+v, want %+v", long, c.wantLong)
			}
			if !spill.Equal(yenOf(c.wantSpill)) {
				t.Errorf("余り = %s, want %d", spill, c.wantSpill)
			}
			if c.wantShort != nil && (!d.ShortOpen || d.Short.N != c.wantShort.N || !d.Short.Budget.Equal(c.wantShort.Budget)) {
				t.Errorf("ショート = %+v（open=%v）, want %+v", d.Short, d.ShortOpen, *c.wantShort)
			}
			if got := DoneForToday(c.cfg, c.placed, c.watchOnly); got != c.wantDone {
				t.Errorf("DoneForToday = %v, want %v", got, c.wantDone)
			}
			if c.cfg.Margin.SpillToLong && !c.watchOnly {
				// 長短の合計は「ロングの枠 + ショートの枠」を超えない
				total := c.placed.LongAmount.Add(c.placed.ShortAmount).
					Add(long.Budget.Mul(decimal.NewFromInt(int64(long.N))))
				for _, pk := range c.shortPicks {
					total = total.Add(pk.Amount())
				}
				if limit := c.cfg.Capital.MaxCapital.Add(c.cfg.Margin.MaxCapital); total.GreaterThan(limit) {
					t.Errorf("長短の合計 %s が枠 %s を超える", total, limit)
				}
			}
		})
	}
}

// 1 回目（建てた分が無い）の件数と予算は、引く前の式（selection.SpillInto / CapByTied）と同じ。
// バックテストと evaluate の再構成が同じ式を使っているので、ここがずれると検証と本番が食い違う。
func TestSizeDayFirstRunMatchesBacktestFormula(t *testing.T) {
	cfg := sizingConfig(true, true)
	for _, used := range []int64{0, 400_000, 1_000_000, 2_999_000} {
		d := SizeDay(SizingInput{Cfg: cfg, Verdict: tradeDay()})
		var picks []selection.Pick
		if used > 0 {
			picks = []selection.Pick{shortPick(used)}
		}
		long, spill, _ := d.WithSpill(picks)
		wantN, wantBudget := selection.SpillInto(3, yenOf(1_000_000), yenOf(1_000_000), yenOf(3_000_000-used), 10)
		if long.N != wantN || !long.Budget.Equal(wantBudget) || !spill.Equal(yenOf(3_000_000-used)) {
			t.Errorf("used %d: (%d, %s, spill %s), want (%d, %s)", used, long.N, long.Budget, spill, wantN, wantBudget)
		}
	}
}

// 建てた銘柄を落とした後に「寄っている」銘柄を落とす。元の気配から落とし直すと
// 建てた銘柄が候補に戻り、同じ日に重ねて建てる。
func TestRankQuotesKeepsExclusionsWithSkipOpened(t *testing.T) {
	quotes := map[string]selection.Quote{
		"A": {Symbol: "A", Opened: true},
		"B": {Symbol: "B"}, // 今日建てた
		"C": {Symbol: "C"}, // 台帳外として返済に回した
		"D": {Symbol: "D"},
	}
	placed := map[string]domain.Side{"B": domain.SideBuy}
	swept := map[string]struct{}{"C": {}}
	for _, skipOpened := range []bool{true, false} {
		kept, opened := RankQuotes(quotes, placed, swept, skipOpened)
		if _, ok := kept["B"]; ok {
			t.Errorf("skip_opened=%v: 建てた銘柄が候補に戻った", skipOpened)
		}
		if _, ok := kept["C"]; ok {
			t.Errorf("skip_opened=%v: 返済に回した銘柄が候補に戻った", skipOpened)
		}
		if _, ok := kept["D"]; !ok {
			t.Errorf("skip_opened=%v: 残すべき銘柄が落ちた", skipOpened)
		}
		_, hasA := kept["A"]
		if skipOpened && (hasA || len(opened) != 1 || opened[0] != "A") {
			t.Errorf("寄っている銘柄の除外: kept=%v opened=%v", kept, opened)
		}
		if !skipOpened && (!hasA || len(opened) != 0) {
			t.Errorf("skip_opened=false で落としている: kept=%v opened=%v", kept, opened)
		}
	}
}

const watchRowsForTest = 5
