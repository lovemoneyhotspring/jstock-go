package engine

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

func runBT(t *testing.T, allBars map[string][]domain.Bar, symbols []string, opts BacktestOptions) (*BacktestStats, error) {
	t.Helper()
	s, err := strategy.Create("sma_cross", map[string]any{"fast": int64(5), "slow": int64(20)})
	if err != nil {
		t.Fatal(err)
	}
	return RunBacktest(
		btSettings(symbols),
		&wbjpcfg.StrategiesConfig{Combiner: "weighted_vote", EntryThreshold: 0.3, ExitThreshold: 0.1},
		[]strategy.Strategy{s},
		map[string]float64{"sma_cross": 1.0},
		strategy.CombineWeightedVote,
		allBars,
		decimal.NewFromInt(1_000_000),
		opts,
	)
}

// --from / --to は売買の対象日だけを絞る。前の足はウォームアップとして残る
// ので、期間を切っても指標が確定しないまま建てることにはならない。
func TestBacktestRespectsDateRange(t *testing.T) {
	bars := btBars("AAA", 200, 0.002)
	allBars := map[string][]domain.Bar{"AAA": bars}

	full, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from := bars[150].Date
	limited, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{Start: from})
	if err != nil {
		t.Fatal(err)
	}

	if limited.Days != 50 {
		t.Errorf("対象日数 = %d, want 50", limited.Days)
	}
	if limited.Days >= full.Days {
		t.Errorf("期間を絞っても日数が減っていません（%d / %d）", limited.Days, full.Days)
	}
}

// 期間に取引日が 1 日も無ければ、黙って空の結果を返さずエラーにする。
func TestBacktestEmptyRangeIsAnError(t *testing.T) {
	allBars := map[string][]domain.Bar{"AAA": btBars("AAA", 60, 0.002)}
	_, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{Start: "2999-01-01"})
	if err == nil {
		t.Fatal("空の期間がエラーになりません")
	}
}

func TestBacktestRejectsUnknownFillModel(t *testing.T) {
	allBars := map[string][]domain.Bar{"AAA": btBars("AAA", 60, 0.002)}
	if _, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{FillModel: "magic"}); err == nil {
		t.Fatal("未知の約定モデルが通ってしまいました")
	}
	if !ValidFillModel("") || !ValidFillModel("open") || !ValidFillModel("intrabar") {
		t.Error("既定と 2 つの約定モデルは通るべき")
	}
}

// 待機資金の利回りを与えると現金に利息が付く。与えなければ無利息のまま。
func TestBacktestAccruesCashInterest(t *testing.T) {
	bars := btBars("AAA", 60, 0.002)
	allBars := map[string][]domain.Bar{"AAA": bars}

	// 年 5% の利回り系列（終値が % で入る）
	yield := make([]domain.Bar, len(bars))
	for i, b := range bars {
		yield[i] = domain.Bar{
			Symbol: "^IRX", Date: b.Date,
			Open: decimal.NewFromInt(5), High: decimal.NewFromInt(5),
			Low: decimal.NewFromInt(5), Close: decimal.NewFromInt(5),
		}
	}

	plain, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	withYield, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{CashYield: yield})
	if err != nil {
		t.Fatal(err)
	}

	if !plain.Interest.IsZero() {
		t.Errorf("利回りを与えていないのに利息 = %s", plain.Interest)
	}
	if !withYield.Interest.IsPositive() {
		t.Errorf("利息 = %s, want > 0", withYield.Interest)
	}
	if !withYield.FinalEquity.GreaterThan(plain.FinalEquity) {
		t.Errorf("利息が資産に乗っていません（%s / %s）", withYield.FinalEquity, plain.FinalEquity)
	}
}

// 表示用の追加指標が空のまま返ってこないこと（Analyze が接続されている）。
func TestBacktestReportsAnalysis(t *testing.T) {
	allBars := map[string][]domain.Bar{"AAA": btBars("AAA", 120, 0.002)}
	stats, err := runBT(t, allBars, []string{"AAA"}, BacktestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"勝率", "シャープレシオ (年率)", "最長ドローダウン期間 (本)"} {
		if _, ok := stats.Analysis[key]; !ok {
			t.Errorf("分析に %q がありません: %v", key, stats.Analysis)
		}
	}
}
