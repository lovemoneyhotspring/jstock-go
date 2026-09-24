package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// planOf は 1 銘柄の ExitPlan の結果を引く。
func planOf(t *testing.T, targets []domain.TargetPosition, symbol string) domain.TargetPosition {
	t.Helper()
	for _, tg := range targets {
		if tg.Symbol == symbol {
			return tg
		}
	}
	t.Fatalf("%s の目標が無い: %+v", symbol, targets)
	return domain.TargetPosition{}
}

// TestExitPlanStopBeatsRunnerHold は W1 の再現。利確済みの残り玉がストップに抵触したら、
// 残り玉の「維持」ではなく手仕舞い（0 株）になる。以前は RunnerTargets が最後に足され、
// 後勝ちの ApplyStopPriority で損切りが打ち消されていた。
func TestExitPlanStopBeatsRunnerHold(t *testing.T) {
	sb := NewStopBook(nil)
	qty0 := dec("100")
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("1000"),
		InitialStopPrice: decPtr("900"), InitialQuantity: &qty0, ScaledOut: true})
	cfg := wbjpcfg.StopsConfig{TakeProfitR: decPtr("2"), TakeProfitFraction: dec("0.5"), TrendExitKind: "sma"}
	targets := sb.ExitPlan(cfg, ExitInputs{
		Closes:     map[string]decimal.Decimal{"7203": dec("990")},
		Quantities: map[string]decimal.Decimal{"7203": dec("50")},
		AsOf:       "2026-09-24",
	})
	if got := planOf(t, targets, "7203"); !got.Quantity.IsZero() {
		t.Errorf("損切りが残り玉の維持に負けた: %+v", got)
	}
	// 戦略が「保有継続」でも損切りが勝つ
	merged := ApplyStopPriority([]domain.TargetPosition{{Symbol: "7203", Quantity: dec("50"), Reason: "保有継続"}}, targets)
	if !merged[0].Quantity.IsZero() {
		t.Errorf("戦略に負けた: %+v", merged[0])
	}
}

// TestExitPlanTakeProfitKeepsReducedQuantity は W1 の再現。同じ回に利確が ScaledOut を立てても、
// 残り玉の「維持」が利確前の株数で利確を打ち消さない（株数は利確後の値）。
func TestExitPlanTakeProfitKeepsReducedQuantity(t *testing.T) {
	sb := NewStopBook(nil)
	qty0 := dec("100")
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("900"),
		InitialStopPrice: decPtr("900"), InitialQuantity: &qty0})
	cfg := wbjpcfg.StopsConfig{TakeProfitR: decPtr("2"), TakeProfitFraction: dec("0.5"), TrendExitKind: "sma"}
	targets := sb.ExitPlan(cfg, ExitInputs{
		Closes:     map[string]decimal.Decimal{"7203": dec("1250")},
		Quantities: map[string]decimal.Decimal{"7203": dec("100")},
		LotSizes:   map[string]decimal.Decimal{"7203": dec("1")},
		AsOf:       "2026-09-24",
	})
	if got := planOf(t, targets, "7203"); !got.Quantity.Equal(dec("50")) {
		t.Errorf("利確後の株数になっていない: %+v", got)
	}
	st, _ := sb.Get("7203")
	if !st.ScaledOut || !st.StopPrice.Equal(dec("1000")) {
		t.Errorf("利確で ScaledOut と建値への引き上げが立っていない: %+v", st)
	}
}

// TestExitPlanTrendExitUsesBars は残り玉の手仕舞い線を渡された足から計算する
// （本番も backtest も同じ ExitPlan を通るので、線の出どころを揃える）。
func TestExitPlanTrendExitUsesBars(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("1000"), ScaledOut: true})
	var bars []domain.Bar
	for _, c := range []string{"1200", "1200", "1200", "1100"} {
		bars = append(bars, domain.Bar{Symbol: "7203", Open: dec(c), High: dec(c), Low: dec(c), Close: dec(c)})
	}
	sma := 3
	cfg := wbjpcfg.StopsConfig{TrendExitSMA: &sma, TrendExitKind: "sma", TakeProfitFraction: dec("0.5")}
	in := ExitInputs{
		Closes:     map[string]decimal.Decimal{"7203": dec("1100")},
		Quantities: map[string]decimal.Decimal{"7203": dec("50")},
		AsOf:       "2026-09-24",
		Bars:       func(string) []domain.Bar { return bars },
	}
	if got := planOf(t, sb.ExitPlan(cfg, in), "7203"); !got.Quantity.IsZero() {
		t.Errorf("移動平均（1166）割れで手仕舞うべき: %+v", got)
	}
	in.Bars = nil
	if got := planOf(t, sb.ExitPlan(cfg, in), "7203"); !got.Quantity.Equal(dec("50")) {
		t.Errorf("線が無ければ維持: %+v", got)
	}
}

// TestApplyStopPriorityOrderIndependent は、ストップ由来の目標の並び順で結果が変わらない。
func TestApplyStopPriorityOrderIndependent(t *testing.T) {
	hold := domain.TargetPosition{Symbol: "7203", Quantity: dec("50"), Reason: "利確後の残り玉を維持"}
	exit := domain.TargetPosition{Symbol: "7203", Quantity: decimal.Zero, Reason: "ストップ抵触"}
	for _, stops := range [][]domain.TargetPosition{{exit, hold}, {hold, exit}} {
		got := ApplyStopPriority(nil, stops)
		if len(got) != 1 || !got[0].Quantity.IsZero() || got[0].Reason != "ストップ抵触" {
			t.Errorf("手仕舞いが勝つべき: %+v", got)
		}
	}
}

// TestEnsureDoesNotRemoveUnheldStops は、非保有のストップを外すのが RetainHeld だけになったこと。
// 以前は Ensure も外していた（外す処理が 2 か所あった）。
func TestEnsureDoesNotRemoveUnheldStops(t *testing.T) {
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("900")})
	sb.EnsureWithOptions(map[string]domain.Position{}, nil, "2026-09-24", EnsureOptions{ATRMultiple: dec("2")})
	if sb.Len() != 1 {
		t.Fatal("Ensure がストップを外した")
	}
	if removed := sb.RetainHeld(map[string]domain.Position{}); len(removed) != 1 || sb.Len() != 0 {
		t.Errorf("RetainHeld が外していない: %v", removed)
	}
}
