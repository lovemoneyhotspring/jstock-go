package basket

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// makeBars は平日だけを並べた日足を作る（営業日の代わり）。
func makeBars(closes []float64, start string) []domain.Bar {
	day, _ := time.Parse("2006-01-02", start)
	bars := make([]domain.Bar, 0, len(closes))
	for _, c := range closes {
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, 1)
		}
		price := decimal.NewFromFloat(c)
		bars = append(bars, domain.Bar{Date: day.Format("2006-01-02"), Open: price, High: price, Low: price, Close: price})
		day = day.AddDate(0, 0, 1)
	}
	return bars
}

func flat(value float64, n int, start string) []domain.Bar {
	closes := make([]float64, n)
	for i := range closes {
		closes[i] = value
	}
	return makeBars(closes, start)
}

// --- 配分表 -------------------------------------------------------------

func TestScheduleAppliesFromTheDayAfterEffective(t *testing.T) {
	schedule, err := FromPairs([]WeightEntry{
		{Effective: "2020-01-10", Weights: map[string]float64{"A": 1.0}},
		{Effective: "2020-04-10", Weights: map[string]float64{"B": 1.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := schedule.At("2020-01-09"); len(got) != 0 {
		t.Errorf("開始日より前は空のはず: %v", got)
	}
	if got := schedule.At("2020-01-10"); len(got) != 0 {
		t.Errorf("開始日当日はまだ前の表のはず: %v", got)
	}
	if got := schedule.At("2020-01-11")["A"]; got != 1.0 {
		t.Errorf("A=1.0 のはず: %v", got)
	}
	if got := schedule.At("2020-04-11")["B"]; got != 1.0 {
		t.Errorf("B=1.0 のはず: %v", got)
	}
	if got := schedule.Symbols(); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Errorf("銘柄一覧が違う: %v", got)
	}
}

func TestBlendKeepsCoreShareFixed(t *testing.T) {
	satellite := Static(map[string]float64{"A": 3.0, "B": 1.0})
	blended, err := satellite.Blend(map[string]float64{"VOO": 1.0}, 0.3)
	if err != nil {
		t.Fatal(err)
	}
	weights := blended.At("2020-06-01")
	for symbol, want := range map[string]float64{"VOO": 0.7, "A": 0.225, "B": 0.075} {
		if math.Abs(weights[symbol]-want) > 1e-9 {
			t.Errorf("%s: got %v want %v", symbol, weights[symbol], want)
		}
	}
}

func TestNegativeWeightIsRejected(t *testing.T) {
	if _, err := FromPairs([]WeightEntry{{Effective: "2020-01-01", Weights: map[string]float64{"A": -1}}}); err == nil {
		t.Fatal("負の比率は弾かれるべき")
	}
}

// --- 計画 ---------------------------------------------------------------

func paydayBases(rows []PlanRow) []int64 {
	var out []int64
	for _, r := range rows {
		if r.Base.IsPositive() {
			out = append(out, r.Base.IntPart())
		}
	}
	return out
}

func TestBudgetIsSplitByWeightOnPayday(t *testing.T) {
	bars := map[string][]domain.Bar{"A": flat(100, 30, "2020-01-01"), "B": flat(50, 30, "2020-01-01")}
	plans, err := BuildBasketPlan(bars, BasketSettings{
		MonthlyBudget: decimal.NewFromInt(100_000),
		Schedule:      Static(map[string]float64{"A": 3, "B": 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for symbol, want := range map[string]int64{"A": 75_000, "B": 25_000} {
		got := paydayBases(plans[symbol])
		if len(got) == 0 {
			t.Fatalf("%s: 入金日がありません", symbol)
		}
		for _, v := range got {
			if v != want {
				t.Errorf("%s: got %d want %d", symbol, v, want)
			}
		}
	}
}

func TestWeightIsRenormalizedWhenASymbolHasNoBarsYet(t *testing.T) {
	bars := map[string][]domain.Bar{
		"A": flat(100, 40, "2020-01-01"),
		"B": flat(50, 20, "2020-01-29"), // 途中から上場
	}
	plans, err := BuildBasketPlan(bars, BasketSettings{
		MonthlyBudget: decimal.NewFromInt(100_000),
		Schedule:      Static(map[string]float64{"A": 1, "B": 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	a := paydayBases(plans["A"])
	if len(a) != 2 || a[0] != 100_000 || a[1] != 50_000 {
		t.Errorf("A の入金日: got %v want [100000 50000]", a)
	}
	if b := paydayBases(plans["B"]); len(b) != 1 || b[0] != 50_000 {
		t.Errorf("B の入金日: got %v want [50000]", b)
	}
}

func TestTacticMultiplierScalesWithWeight(t *testing.T) {
	closes := make([]float64, 500)
	for i := range closes {
		closes[i] = 200.0 - 100.0*float64(i)/499.0
	}
	down := makeBars(closes, "2020-01-01")
	bearStack, err := tactics.NewBearStack(3, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	plans, err := BuildBasketPlan(
		map[string][]domain.Bar{"A": down, "B": down},
		BasketSettings{
			MonthlyBudget: decimal.NewFromInt(100_000),
			Schedule:      Static(map[string]float64{"A": 1, "B": 1}),
			Tactic:        bearStack,
		})
	if err != nil {
		t.Fatal(err)
	}
	sum := func(rows []PlanRow) int64 {
		total := int64(0)
		for _, r := range rows {
			total += r.Extra.IntPart()
		}
		return total
	}
	if sum(plans["A"]) == 0 {
		t.Fatal("下降局面で増額が出ていない")
	}
	if sum(plans["A"]) != sum(plans["B"]) {
		t.Errorf("同じ足なら増額も等しいはず: %d != %d", sum(plans["A"]), sum(plans["B"]))
	}
}

func TestSymbolsWithZeroWeightAreDropped(t *testing.T) {
	bars := map[string][]domain.Bar{"A": flat(100, 30, "2020-01-01"), "B": flat(50, 30, "2020-01-01")}
	plans, err := BuildBasketPlan(bars, BasketSettings{
		MonthlyBudget: decimal.NewFromInt(100_000),
		Schedule:      Static(map[string]float64{"A": 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("配分 0 の銘柄は落とすはず: %v", plans)
	}
	if _, ok := plans["A"]; !ok {
		t.Errorf("A が残るはず: %v", plans)
	}
}

// --- 傾斜 ---------------------------------------------------------------

func TestDrawdownTiltFavorsTheDeeperFall(t *testing.T) {
	tilt, err := NewDrawdownTilt(2.0, 10)
	if err != nil {
		t.Fatal(err)
	}
	bars := makeBars([]float64{100, 100, 70}, "2020-01-01")
	factors := tilt.Factor(bars)
	// 30% 下げているので 1 + 2×0.3 = 1.6
	if math.Abs(factors[2]-1.6) > 1e-9 {
		t.Errorf("係数が違う: %v", factors[2])
	}
	if factors[0] != 1.0 {
		t.Errorf("高値のままなら 1.0: %v", factors[0])
	}
}

func TestDrawdownTiltRejectsBadParameters(t *testing.T) {
	if _, err := NewDrawdownTilt(0, 10); err == nil {
		t.Error("strength=0 は弾かれるべき")
	}
	if _, err := NewDrawdownTilt(2, 1); err == nil {
		t.Error("lookback=1 は弾かれるべき")
	}
}

// --- 検証 ---------------------------------------------------------------

func TestXIRRMatchesAKnownGrowthRate(t *testing.T) {
	rate := XIRR([]string{"2021-01-01", "2022-01-01"}, []float64{-100, 0}, 110)
	if math.Abs(rate-0.10) > 1e-4 {
		t.Errorf("got %v want 0.10", rate)
	}
}

func TestXIRRReturnsZeroWithoutASignChange(t *testing.T) {
	if rate := XIRR([]string{"2021-01-01", "2022-01-01"}, []float64{-100, -100}, 0); rate != 0 {
		t.Errorf("解が無いときは 0: %v", rate)
	}
}

func TestFlatMarketReturnsContributedAmount(t *testing.T) {
	bars := map[string][]domain.Bar{"A": flat(100, 60, "2020-01-01"), "B": flat(50, 60, "2020-01-01")}
	settings := BasketSettings{
		MonthlyBudget: decimal.NewFromInt(100_000),
		Schedule:      Static(map[string]float64{"A": 1, "B": 1}),
	}
	plans, err := BuildBasketPlan(bars, settings)
	if err != nil {
		t.Fatal(err)
	}
	result, err := SimulateBasket(bars, plans, bars["A"])
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(result.Basket.TerminalValue-result.Basket.Contributed) > 1.0 {
		t.Errorf("横ばいなら期末＝投入額: %v != %v", result.Basket.TerminalValue, result.Basket.Contributed)
	}
	if result.Benchmark == nil {
		t.Fatal("基準銘柄の結果が無い")
	}
	if math.Abs(result.Benchmark.TerminalValue-result.Benchmark.Contributed) > 1.0 {
		t.Errorf("基準も横ばいなら期末＝投入額: %v", result.Benchmark)
	}
	if len(result.Symbols) != 2 {
		t.Errorf("投下した銘柄は 2 つ: %v", result.Symbols)
	}
}

func TestSimulateBasketRejectsEmptyPlans(t *testing.T) {
	if _, err := SimulateBasket(map[string][]domain.Bar{}, map[string][]PlanRow{}, nil); err == nil {
		t.Fatal("空の計画表は弾かれるべき")
	}
}

func TestRisingMarketBeatsContributions(t *testing.T) {
	closes := make([]float64, 260)
	for i := range closes {
		closes[i] = 100.0 * (1.0 + float64(i)/260.0)
	}
	bars := map[string][]domain.Bar{"A": makeBars(closes, "2020-01-01")}
	plans, err := BuildBasketPlan(bars, BasketSettings{
		MonthlyBudget: decimal.NewFromInt(100_000),
		Schedule:      Static(map[string]float64{"A": 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := SimulateBasket(bars, plans, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Basket.TotalReturn() <= 0 {
		t.Errorf("上昇局面では総リターンが正のはず: %v", result.Basket.TotalReturn())
	}
	if result.Basket.XIRR <= 0 {
		t.Errorf("上昇局面では XIRR が正のはず: %v", result.Basket.XIRR)
	}
	if result.Basket.MaxDrawdown != 0 {
		t.Errorf("単調上昇なら最大DDは 0: %v", result.Basket.MaxDrawdown)
	}
}
