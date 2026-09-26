package selection

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func lgbmSignal(t *testing.T) config.Signal {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	cfg := config.Default()
	cfg.Signal.RankBy = config.RankByLGBM
	cfg.Signal.Model = filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "daytrade", "models", "lgbm_rank.txt")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg.Signal
}

// rank_by = lgbm は予測値の高い順に並べ、既存規則の順位を RuleRank に残す。
func TestRankByLGBMReordersAndKeepsRuleRank(t *testing.T) {
	sig := lgbmSignal(t)
	vol := 0.02
	var candidates []universe.Candidate
	quotes := map[string]Quote{}
	for i, s := range []string{"1000", "2000", "3000", "4000", "5000", "6000"} {
		ret := float64(i-3) * 0.02
		c := candidate(s, 1000, &vol)
		c.TurnoverMed, c.MktCap = float64(i+1)*3e8, float64(6-i)*1e5
		c.Ret1, c.Ret5 = &ret, &ret
		candidates = append(candidates, c)
		quotes[s] = quote(s, 1000*(1-0.01*float64(i+1)))
	}
	ranked := Rank(candidates, quotes, sig)
	if len(ranked) != 6 {
		t.Fatalf("順位表 %d 件", len(ranked))
	}
	ruleOrder := map[int]string{}
	for i, r := range ranked {
		if r.Score == nil || r.Rank != i+1 {
			t.Fatalf("%d 行目: 予測値 %v・順位 %d", i, r.Score, r.Rank)
		}
		if i > 0 && *r.Score > *ranked[i-1].Score {
			t.Fatalf("予測値の高い順になっていない: %v > %v", *r.Score, *ranked[i-1].Score)
		}
		ruleOrder[r.RuleRank] = r.Symbol
	}
	// 既存規則はギャップの深い順（6000 が −6% で先頭）
	for rank, want := range map[int]string{1: "6000", 6: "1000"} {
		if ruleOrder[rank] != want {
			t.Errorf("既存規則の %d 位 = %s, want %s", rank, ruleOrder[rank], want)
		}
	}

	opts := PickOptions{N: 2, Budget: decimal.NewFromInt(1_000_000), Weighting: "equal", Side: domain.SideBuy}
	picks := PickFrom(ranked, opts)
	rule := RulePicks(ranked, opts, picks)
	if len(rule) != 2 || rule[0].Symbol != "6000" || rule[1].Symbol != "5000" {
		t.Errorf("既存規則の選定 = %v", rule)
	}
}

// 既存規則で並べた日は RuleRank = Rank、RulePicks は選定そのもの。
func TestRulePicksWithoutModel(t *testing.T) {
	cfg := config.Default().Signal
	candidates := []universe.Candidate{candidate("1000", 1000, nil), candidate("2000", 1000, nil)}
	quotes := map[string]Quote{"1000": quote("1000", 950), "2000": quote("2000", 980)}
	ranked := Rank(candidates, quotes, cfg)
	for _, r := range ranked {
		if r.RuleRank != r.Rank || r.Score != nil {
			t.Errorf("%s: rule_rank %d / rank %d / score %v", r.Symbol, r.RuleRank, r.Rank, r.Score)
		}
	}
	opts := PickOptions{N: 1, Budget: decimal.NewFromInt(1_000_000), Weighting: "equal", Side: domain.SideBuy}
	picks := PickFrom(ranked, opts)
	if got := RulePicks(ranked, opts, picks); len(got) != 1 || got[0].Symbol != picks[0].Symbol {
		t.Errorf("RulePicks = %v, want %v", got, picks)
	}
}

// TryRank は並べ替えの失敗（読めないモデル）を誤りで返し、パニックを外に出さない。
func TestTryRankReportsFailure(t *testing.T) {
	sig := lgbmSignal(t)
	candidates := []universe.Candidate{candidate("1000", 1000, nil)}
	quotes := map[string]Quote{"1000": quote("1000", 950)}
	if got, err := TryRank(candidates, quotes, sig); err != nil || len(got) != 1 || got[0].Score == nil {
		t.Fatalf("読めるモデルで %v / %v", got, err)
	}
	sig.Model = filepath.Join(t.TempDir(), "none.txt")
	if got, err := TryRank(candidates, quotes, sig); err == nil || got != nil {
		t.Errorf("読めないモデルで誤りにならない: %v / %v", got, err)
	}
	// 候補が 0 件ならモデルに触れないので誤りにしない
	if _, err := TryRank(nil, quotes, sig); err != nil {
		t.Errorf("候補 0 件で誤り: %v", err)
	}
}

// モデルの束の売買代金の下限（LBZ2 は 5 億）より小さい候補は並べた後に外し、特徴量は外す前の全候補で作る。
func TestRankBySpecDropsBelowMinTurnover(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	sig := lgbmSignal(t)
	sig.Model = filepath.Join(filepath.Dir(file), "..", "..", "..", "config", "daytrade", "models", "lbz2", "manifest.json")
	vol := 0.02
	var candidates []universe.Candidate
	quotes := map[string]Quote{}
	for i, s := range []string{"1000", "2000", "3000", "4000", "5000", "6000"} {
		ret, rd2 := -0.01, float64(i-3)*0.01
		c := candidate(s, 1000, &vol)
		c.TurnoverMed, c.MktCap = float64(i+1)*2e8, 1e5 // 2・4・6・8・10・12 億
		c.Ret1, c.RetD2 = &ret, &rd2
		candidates = append(candidates, c)
		quotes[s] = quote(s, 1000*(1-0.01*float64(i+1)))
	}
	ranked := Rank(candidates, quotes, sig)
	if len(ranked) != 4 {
		t.Fatalf("順位表 %d 件、5 億以上の 4 件のはず", len(ranked))
	}
	for i, r := range ranked {
		if r.Turnover < 5e8 || r.Rank != i+1 || r.Score == nil {
			t.Errorf("%d 行目 %s: 売買代金 %v・順位 %d・予測値 %v", i, r.Symbol, r.Turnover, r.Rank, r.Score)
		}
		// 既存規則の順位は外す前の 6 件の中の順位のまま
		if r.RuleRank < 1 || r.RuleRank > 6 {
			t.Errorf("%s の既存規則の順位 %d", r.Symbol, r.RuleRank)
		}
	}
}
