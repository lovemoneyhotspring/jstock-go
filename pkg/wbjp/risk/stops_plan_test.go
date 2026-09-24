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
	// 本番（AssumeFilled 偽）は売りを出しただけでは確定しない
	if st, _ := sb.Get("7203"); st.ScaledOut || !st.StopPrice.Equal(dec("900")) {
		t.Errorf("約定を見る前に利確を確定した: %+v", st)
	}

	// backtest（AssumeFilled）は決めた時点で確定し、同じ回の残り玉の「維持」（100 株）が
	// 利確を打ち消さない
	sb2 := NewStopBook(nil)
	sb2.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("900"),
		InitialStopPrice: decPtr("900"), InitialQuantity: &qty0})
	targets = sb2.ExitPlan(cfg, ExitInputs{
		Closes:       map[string]decimal.Decimal{"7203": dec("1250")},
		Quantities:   map[string]decimal.Decimal{"7203": dec("100")},
		LotSizes:     map[string]decimal.Decimal{"7203": dec("1")},
		AsOf:         "2026-09-24",
		AssumeFilled: true,
	})
	if got := planOf(t, targets, "7203"); !got.Quantity.Equal(dec("50")) {
		t.Errorf("backtest: 利確後の株数になっていない: %+v", got)
	}
	if st, _ := sb2.Get("7203"); !st.ScaledOut || !st.StopPrice.Equal(dec("1000")) {
		t.Errorf("backtest: 利確で ScaledOut と建値への引き上げが立っていない: %+v", st)
	}
}

// TestExitPlanTakeProfitRetriesUntilFilled は 2026-09-24 のレビューの再現。本番で利確の売りが
// 約定しなかった（指値の失効・当日買付の柵・発注の上限・発注失敗）とき、次の回にも同じ残り株数の
// 利確が出る。以前は売りを出した回に ScaledOut が保存され、二度と利確が出ずに残り玉の「維持」が
// 全株を固定していた。保有が減ったのを見た回に ScaledOut と建値への引き上げを確定する。
func TestExitPlanTakeProfitRetriesUntilFilled(t *testing.T) {
	qty0 := dec("100")
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("900"),
		InitialStopPrice: decPtr("900"), InitialQuantity: &qty0})
	cfg := wbjpcfg.StopsConfig{TakeProfitR: decPtr("2"), TakeProfitFraction: dec("0.5"), TrendExitKind: "sma"}
	in := func(held string) ExitInputs {
		return ExitInputs{
			Closes:     map[string]decimal.Decimal{"7203": dec("1250")},
			Quantities: map[string]decimal.Decimal{"7203": dec(held)},
			LotSizes:   map[string]decimal.Decimal{"7203": dec("1")},
			AsOf:       "2026-09-24",
		}
	}

	// 1 回目・2 回目（売れなかった）とも同じ 50 株の目標
	for i := 1; i <= 2; i++ {
		if got := planOf(t, sb.ExitPlan(cfg, in("100")), "7203"); !got.Quantity.Equal(dec("50")) {
			t.Fatalf("%d 回目: 利確の目標（残り 50 株）が出ない: %+v", i, got)
		}
		if st, _ := sb.Get("7203"); st.ScaledOut {
			t.Fatalf("%d 回目: 約定前に ScaledOut が立った", i)
		}
	}

	// 一部だけ約定（70 株）→ まだ確定しない。目標は同じ 50 株（InitialQuantity 基準で冪等）
	if got := planOf(t, sb.ExitPlan(cfg, in("70")), "7203"); !got.Quantity.Equal(dec("50")) {
		t.Fatalf("一部約定: 残り 50 株の目標: %+v", got)
	}

	// 50 株まで減った → 確定（建値へ引き上げ）。利確は出さず、残り玉を維持
	got := planOf(t, sb.ExitPlan(cfg, in("50")), "7203")
	if !got.Quantity.Equal(dec("50")) || got.Reason != "利確後の残り玉を維持（トレンド追従中）" {
		t.Errorf("約定後は残り玉の維持: %+v", got)
	}
	if st, _ := sb.Get("7203"); !st.ScaledOut || !st.StopPrice.Equal(dec("1000")) {
		t.Errorf("約定を見て ScaledOut と建値への引き上げを確定していない: %+v", st)
	}
}

// TestExitPlanTakeProfitNoConfirmBelowTarget は、含み益が target に届いていない建玉を株数が
// 減っただけで利確済みにしない（地合いで減らした含み損の建玉のストップを建値へ上げない）。
func TestExitPlanTakeProfitNoConfirmBelowTarget(t *testing.T) {
	qty0 := dec("100")
	sb := NewStopBook(nil)
	sb.Set(Stop{Symbol: "7203", EntryPrice: dec("1000"), StopPrice: dec("900"),
		InitialStopPrice: decPtr("900"), InitialQuantity: &qty0})
	cfg := wbjpcfg.StopsConfig{TakeProfitR: decPtr("2"), TakeProfitFraction: dec("0.5"), TrendExitKind: "sma"}
	sb.ExitPlan(cfg, ExitInputs{
		Closes:     map[string]decimal.Decimal{"7203": dec("950")},
		Quantities: map[string]decimal.Decimal{"7203": dec("50")},
		LotSizes:   map[string]decimal.Decimal{"7203": dec("1")},
		AsOf:       "2026-09-24",
	})
	if st, _ := sb.Get("7203"); st.ScaledOut || !st.StopPrice.Equal(dec("900")) {
		t.Errorf("含み損の建玉を利確済みにした: %+v", st)
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
