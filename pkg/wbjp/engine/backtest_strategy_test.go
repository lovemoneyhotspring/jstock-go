package engine

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

// btBars は上昇トレンドの日足を n 本作る。
func btBars(symbol string, n int, daily float64) []domain.Bar {
	bars := make([]domain.Bar, 0, n)
	price := 1000.0
	cur := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for len(bars) < n {
		if cur.Weekday() == time.Saturday || cur.Weekday() == time.Sunday {
			cur = cur.AddDate(0, 0, 1)
			continue
		}
		open := price
		price = price * (1 + daily) * (1 + 0.01*math.Sin(float64(len(bars))/3))
		bars = append(bars, domain.Bar{
			Symbol: symbol,
			Date:   cur.Format("2006-01-02"),
			Open:   decimal.NewFromFloat(open),
			High:   decimal.NewFromFloat(math.Max(open, price) * 1.01),
			Low:    decimal.NewFromFloat(math.Min(open, price) * 0.99),
			Close:  decimal.NewFromFloat(price),
			Volume: decimal.NewFromFloat(1_000_000),
		})
		cur = cur.AddDate(0, 0, 1)
	}
	return bars
}

func btSettings(symbols []string) *wbjpcfg.SettingsFile {
	return &wbjpcfg.SettingsFile{
		Universe: wbjpcfg.UniverseConfig{Market: "JP", Symbols: symbols},
		Risk: wbjpcfg.RiskConfig{
			MaxOrderValue:     decimal.NewFromInt(10_000_000),
			MaxOrdersPerDay:   100,
			MaxDailyLoss:      decimal.NewFromInt(10_000_000),
			MaxPositionWeight: decimal.NewFromFloat(0.5),
			MaxGrossExposure:  decimal.NewFromFloat(0.95),
		},
		Sizing: wbjpcfg.SizingConfig{Method: "equal_weight", MaxPositions: 3, ATRStopMultiple: decimal.NewFromInt(2)},
	}
}

// バックテストが新インターフェースの戦略で最後まで通ること。
func TestRunBacktestWithCrossSectionalStrategy(t *testing.T) {
	symbols := []string{"AAA", "BBB"}
	allBars := map[string][]domain.Bar{
		"AAA": btBars("AAA", 200, 0.004),
		"BBB": btBars("BBB", 200, 0.001),
	}

	s, err := strategy.Create("sma_cross", map[string]any{"fast": int64(5), "slow": int64(20)})
	if err != nil {
		t.Fatal(err)
	}

	stats, err := RunBacktest(
		btSettings(symbols),
		&wbjpcfg.StrategiesConfig{Combiner: "weighted_vote", EntryThreshold: 0.3, ExitThreshold: 0.1},
		[]strategy.Strategy{s},
		map[string]float64{"sma_cross": 1.0},
		strategy.CombineWeightedVote,
		allBars,
		decimal.NewFromInt(1_000_000),
	)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalFills == 0 {
		t.Error("上昇トレンドで1件も約定しないのはおかしい")
	}
	if stats.FinalEquity.IsNegative() {
		t.Errorf("最終資産 = %s", stats.FinalEquity)
	}
}

// 指標を全履歴で一度だけ計算しているので、日数を倍にしても実行時間は
// 二乗では増えない。ここが崩れると長期のバックテストが実用外になる。
func TestBacktestScalesLinearlyInDays(t *testing.T) {
	if testing.Short() {
		t.Skip("時間がかかるため -short では飛ばす")
	}
	s, err := strategy.Create("sma_cross", map[string]any{"fast": int64(5), "slow": int64(20)})
	if err != nil {
		t.Fatal(err)
	}

	run := func(n int) time.Duration {
		allBars := map[string][]domain.Bar{"AAA": btBars("AAA", n, 0.002)}
		start := time.Now()
		_, err := RunBacktest(
			btSettings([]string{"AAA"}),
			&wbjpcfg.StrategiesConfig{Combiner: "weighted_vote", EntryThreshold: 0.3, ExitThreshold: 0.1},
			[]strategy.Strategy{s},
			map[string]float64{"sma_cross": 1.0},
			strategy.CombineWeightedVote,
			allBars,
			decimal.NewFromInt(1_000_000),
		)
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}

	small := run(500)
	large := run(2000)
	// 二乗なら 16 倍。線形なら 4 倍。余裕をみて 10 倍を上限にする。
	if large > small*10 && large > 200*time.Millisecond {
		t.Errorf("日数 4 倍で %v → %v。指標が日ごとに再計算されている可能性", small, large)
	}
}
