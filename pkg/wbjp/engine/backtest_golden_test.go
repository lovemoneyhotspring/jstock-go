package engine

// RunBacktest の特性テスト（入力→出力の golden）。
//
// 分割のリファクタリングで振る舞いが変わらないことを確かめるためのもので、**今の挙動をそのまま
// 固定する**。値が正しいかどうかは見ていない。条件ごとに BacktestStats の全項目を書き出し、
// testdata/backtest_golden/*.golden と突き合わせる。
//
// 期待値を作り直すとき: go test ./pkg/wbjp/engine -run TestBacktestGolden -update-backtest-golden

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
)

var updateBacktestGolden = flag.Bool("update-backtest-golden", false, "RunBacktest の特性テストの期待値（testdata/backtest_golden）を書き直す")

// goldenBars は上げ下げの波を持つ日足。amp と period で銘柄ごとに形を変える。
func goldenBars(symbol string, n int, drift, amp, period float64) []domain.Bar {
	bars := make([]domain.Bar, 0, n)
	price := 1000.0
	cur := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for len(bars) < n {
		if cur.Weekday() == time.Saturday || cur.Weekday() == time.Sunday {
			cur = cur.AddDate(0, 0, 1)
			continue
		}
		i := float64(len(bars))
		open := price * (1 + 0.003*math.Cos(i/2))
		price = price * (1 + drift) * (1 + amp*math.Sin(i/period))
		bars = append(bars, domain.Bar{
			Symbol: symbol,
			Date:   cur.Format("2006-01-02"),
			Open:   decimal.NewFromFloat(open).Round(1),
			High:   decimal.NewFromFloat(math.Max(open, price) * 1.012).Round(1),
			Low:    decimal.NewFromFloat(math.Min(open, price) * 0.988).Round(1),
			Close:  decimal.NewFromFloat(price).Round(1),
			Volume: decimal.NewFromFloat(1_000_000),
		})
		cur = cur.AddDate(0, 0, 1)
	}
	return bars
}

// dropDates は bars から指定の添字の日を抜く（その銘柄だけ休んだ日を作る）。
func dropDates(bars []domain.Bar, idx ...int) []domain.Bar {
	skip := make(map[int]bool, len(idx))
	for _, i := range idx {
		skip[i] = true
	}
	out := make([]domain.Bar, 0, len(bars))
	for i, b := range bars {
		if !skip[i] {
			out = append(out, b)
		}
	}
	return out
}

func goldenUniverse() map[string][]domain.Bar {
	return map[string][]domain.Bar{
		"AAA": goldenBars("AAA", 260, 0.002, 0.012, 3),
		"BBB": dropDates(goldenBars("BBB", 260, -0.001, 0.02, 5), 40, 41, 120),
		"CCC": goldenBars("CCC", 260, 0.0005, 0.03, 7),
		"IDX": goldenBars("IDX", 260, 0.0008, 0.008, 11),
	}
}

func goldenStrategies(t *testing.T) ([]strategy.Strategy, map[string]float64) {
	t.Helper()
	s1, err := strategy.Create("sma_cross", map[string]any{"fast": int64(5), "slow": int64(20)})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := strategy.Create("sma_cross", map[string]any{"fast": int64(3), "slow": int64(10)})
	if err != nil {
		t.Fatal(err)
	}
	return []strategy.Strategy{s1, s2}, map[string]float64{"sma_cross": 1.0}
}

func intp(v int) *int { return &v }

func decp(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func formatStats(stats *BacktestStats, err error) string {
	if err != nil {
		return "error: " + err.Error() + "\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "InitialEquity %s\n", stats.InitialEquity)
	fmt.Fprintf(&sb, "FinalEquity %s\n", stats.FinalEquity)
	fmt.Fprintf(&sb, "TotalReturn %s\n", stats.TotalReturn)
	fmt.Fprintf(&sb, "MaxDrawdown %s\n", stats.MaxDrawdown)
	fmt.Fprintf(&sb, "TotalFills %d\n", stats.TotalFills)
	fmt.Fprintf(&sb, "SellFills %d\n", stats.SellFills)
	fmt.Fprintf(&sb, "Days %d\n", stats.Days)
	fmt.Fprintf(&sb, "WinningTrades %d\n", stats.WinningTrades)
	fmt.Fprintf(&sb, "LosingTrades %d\n", stats.LosingTrades)
	fmt.Fprintf(&sb, "WinRate %v\n", stats.WinRate)
	fmt.Fprintf(&sb, "Interest %s\n", stats.Interest)
	keys := make([]string, 0, len(stats.Analysis))
	for k := range stats.Analysis {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "Analysis[%s] %s\n", k, stats.Analysis[k])
	}
	return sb.String()
}

func TestBacktestGolden(t *testing.T) {
	symbols := []string{"AAA", "BBB", "CCC"}
	allBars := goldenUniverse()
	strats, weights := goldenStrategies(t)

	// 利回りは飛び飛びに与える（持ち越しの分岐を通す）
	var yield []domain.Bar
	for i, b := range allBars["AAA"] {
		if i%3 != 0 {
			continue
		}
		v := decimal.NewFromFloat(4 + float64(i%7)*0.25)
		yield = append(yield, domain.Bar{Symbol: "^IRX", Date: b.Date, Open: v, High: v, Low: v, Close: v})
	}
	// 月曜を休みにする営業日判定（時間切れの数え方の分岐）
	noMonday := func(d time.Time) bool {
		wd := d.Weekday()
		return wd != time.Saturday && wd != time.Sunday && wd != time.Monday
	}

	base := func() *wbjpcfg.SettingsFile { return btSettings(symbols) }

	cases := []struct {
		name  string
		set   func() *wbjpcfg.SettingsFile
		strat *wbjpcfg.StrategiesConfig
		cash  decimal.Decimal
		opts  BacktestOptions
	}{
		{name: "base_open", set: base},
		{name: "intrabar", set: base, opts: BacktestOptions{FillModel: "intrabar"}},
		{name: "zero_cash_default", set: base, cash: decimal.Zero},
		{name: "range", set: base, opts: BacktestOptions{Start: "2024-03-01", End: "2024-09-30"}},
		{name: "range_end_only", set: base, opts: BacktestOptions{End: "2024-06-28"}},
		{name: "empty_range", set: base, opts: BacktestOptions{Start: "2030-01-01", End: "2030-12-31"}},
		{name: "bad_fill_model", set: base, opts: BacktestOptions{FillModel: "close"}},
		{name: "cash_yield", set: base, opts: BacktestOptions{CashYield: yield}},
		{name: "lot_override", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Universe.LotSizeOverrides = map[string]int{"AAA": 10, "BBB": 0, "CCC": 1000}
			return s
		}},
		{name: "regime", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Regime = wbjpcfg.RegimeConfig{
				Enabled: true, Benchmark: "IDX", SMALong: 60, SMAMid: 20, SlopeLookback: 10,
				ExposureBull: decimal.NewFromInt(1), ExposureCaution: decimal.NewFromFloat(0.5), ExposureBear: decimal.Zero,
			}
			return s
		}},
		{name: "regime_no_benchmark", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Regime = wbjpcfg.RegimeConfig{
				Enabled: true, Benchmark: "", SMALong: 60, SMAMid: 20, SlopeLookback: 10,
				ExposureBull: decimal.NewFromInt(1), ExposureCaution: decimal.NewFromFloat(0.5), ExposureBear: decimal.NewFromFloat(0.2),
			}
			return s
		}},
		{name: "stops_full", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Stops = wbjpcfg.StopsConfig{
				Trailing:           true,
				BreakevenAfterR:    decp("1"),
				StaleExitDays:      intp(8),
				MaxHoldDays:        intp(25),
				InitialStopPct:     decp("0.05"),
				TakeProfitR:        decp("1.5"),
				TakeProfitFraction: decimal.RequireFromString("0.5"),
			}
			return s
		}, opts: BacktestOptions{TradingDay: noMonday}},
		{name: "stops_trend_exit", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Stops = wbjpcfg.StopsConfig{
				TrendExitSMA:        intp(10),
				TrendExitAlways:     true,
				TrailingATRMultiple: decp("3"),
			}
			return s
		}, opts: BacktestOptions{FillModel: "intrabar"}},
		{name: "tight_risk", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Risk.MaxOrdersPerDay = 1
			s.Risk.MaxDailyLoss = decimal.NewFromInt(20_000)
			s.Risk.MaxOrderValue = decimal.NewFromInt(900_000)
			return s
		}},
		{name: "last_symbol_unknown", set: func() *wbjpcfg.SettingsFile {
			// 足の無い銘柄がユニバースに混ざっている
			return btSettings([]string{"AAA", "ZZZ"})
		}},
		{name: "bad_sizer", set: func() *wbjpcfg.SettingsFile {
			s := base()
			s.Sizing.Method = "no_such_method"
			return s
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			strat := c.strat
			if strat == nil {
				strat = &wbjpcfg.StrategiesConfig{Combiner: "weighted_vote", EntryThreshold: 0.3, ExitThreshold: 0.1}
			}
			cash := c.cash
			if c.name != "zero_cash_default" {
				cash = decimal.NewFromInt(3_000_000)
			}
			stats, err := RunBacktest(c.set(), strat, strats, weights, strategy.CombineWeightedVote,
				allBars, cash, c.opts)
			got := formatStats(stats, err)

			path := filepath.Join("testdata", "backtest_golden", c.name+".golden")
			if *updateBacktestGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("期待値がありません（-update-backtest-golden で作る）: %v", rerr)
			}
			if got != string(want) {
				t.Errorf("出力が期待値と違います\n--- got\n%s--- want\n%s", got, want)
			}
		})
	}
}
