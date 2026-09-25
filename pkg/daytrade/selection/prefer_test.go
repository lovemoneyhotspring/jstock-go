package selection

import (
	"fmt"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// preferDay は gap の小さい順に A1, A2, … と並ぶ候補（i 番目のギャップは −(10−i)% 相当）。
// sectors[i] が業種、stoch[i] がストキャス RSI（負なら値なし）。
func preferDay(sectors []string, stoch []float64) ([]universe.Candidate, map[string]Quote) {
	var cands []universe.Candidate
	quotes := map[string]Quote{}
	for i := range sectors {
		sym := fmt.Sprintf("A%d", i+1)
		c := candidate(sym, 1000, nil)
		c.Sector = sectors[i]
		if stoch[i] >= 0 {
			v := stoch[i]
			c.StochRSI14 = &v
		}
		cands = append(cands, c)
		quotes[sym] = quote(sym, 900+float64(i)*5)
	}
	return cands, quotes
}

func symbols(r []Ranked) []string {
	out := make([]string, len(r))
	for i := range r {
		out[i] = r[i].Symbol
	}
	return out
}

func TestPreferMovesOversoldToFrontWithinPool(t *testing.T) {
	cfg := config.Default().Signal
	cfg.Prefer = config.Prefer{Indicator: config.PreferStochRSI, Max: 0.2, Pool: 3}
	cands, quotes := preferDay(
		[]string{"s1", "s2", "s3", "s4", "s5"},
		[]float64{0.9, 0.5, 0.1, 0.05, -1}, // A3 は範囲内で対象、A4 は範囲（3 位）の外、A5 は値なし
	)
	got := Rank(cands, quotes, cfg)
	want := []string{"A3", "A1", "A2", "A4", "A5"}
	if fmt.Sprint(symbols(got)) != fmt.Sprint(want) {
		t.Fatalf("並び = %v, want %v", symbols(got), want)
	}
	if !got[0].Preferred || got[1].Preferred || got[3].Preferred {
		t.Errorf("Preferred の印がずれている: %+v", got)
	}
	for i, r := range got {
		if r.Rank != i+1 {
			t.Errorf("%s の Rank = %d, want %d", r.Symbol, r.Rank, i+1)
		}
	}
	if got[0].RuleRank != 3 {
		t.Errorf("RuleRank は元の順位のまま: %d", got[0].RuleRank)
	}
}

// 範囲は業種の上限を通る銘柄だけで数え、同じ業種の 2 番手は対象にしない（検証と同じ）。
func TestPreferCountsPoolAfterSectorCap(t *testing.T) {
	cfg := config.Default().Signal
	cfg.MaxPerSector = 1
	cfg.Prefer = config.Prefer{Indicator: config.PreferStochRSI, Max: 0.2, Pool: 2}
	cands, quotes := preferDay(
		[]string{"s1", "s1", "s2", "s3"},
		[]float64{0.9, 0.1, 0.1, 0.1}, // A2 は s1 の 2 番手（上限で落ちる）→ 対象外。範囲は A1・A3 の 2 本
	)
	got := Rank(cands, quotes, cfg)
	want := []string{"A3", "A1", "A2", "A4"}
	if fmt.Sprint(symbols(got)) != fmt.Sprint(want) {
		t.Fatalf("並び = %v, want %v", symbols(got), want)
	}
	if got[2].Preferred || got[3].Preferred {
		t.Errorf("業種の 2 番手・範囲の外に印が付いた: %+v", got)
	}
}

// 業種が空の銘柄は上限に数えない（candidatePool と同じ）。
func TestPreferEmptySectorIsNotCapped(t *testing.T) {
	cfg := config.Default().Signal
	cfg.MaxPerSector = 1
	cfg.Prefer = config.Prefer{Indicator: config.PreferStochRSI, Max: 0.2, Pool: 3}
	cands, quotes := preferDay([]string{"", "", "", ""}, []float64{0.9, 0.9, 0.1, 0.1})
	got := Rank(cands, quotes, cfg)
	want := []string{"A3", "A1", "A2", "A4"}
	if fmt.Sprint(symbols(got)) != fmt.Sprint(want) {
		t.Fatalf("並び = %v, want %v", symbols(got), want)
	}
}

func TestPreferRSI2AndDisabled(t *testing.T) {
	cfg := config.Default().Signal
	cands, quotes := preferDay([]string{"s1", "s2", "s3"}, []float64{-1, -1, -1})
	r2 := []float64{50, 8, 12}
	for i := range cands {
		v := r2[i]
		cands[i].RSI2 = &v
	}
	base := symbols(Rank(cands, quotes, cfg)) // 優先なし
	if fmt.Sprint(base) != "[A1 A2 A3]" {
		t.Fatalf("優先なしの並び = %v", base)
	}
	cfg.Prefer = config.Prefer{Indicator: config.PreferRSI2, Max: 10, Pool: 20}
	if got := symbols(Rank(cands, quotes, cfg)); fmt.Sprint(got) != "[A2 A1 A3]" {
		t.Fatalf("RSI(2) ≤ 10 の並び = %v", got)
	}
	// 指標の値が無い（古い plan）なら元の並びのまま
	cfg.Prefer.Indicator = config.PreferStochRSI
	cfg.Prefer.Max = 0.2
	if got := symbols(Rank(cands, quotes, cfg)); fmt.Sprint(got) != fmt.Sprint(base) {
		t.Fatalf("値が無いのに並びが変わった: %v", got)
	}
}
