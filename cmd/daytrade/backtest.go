package main

import (
	"fmt"
	"time"

	dtbacktest "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newBacktestCmd() *cobra.Command {
	var sinceFlag, untilFlag string
	var tradesFlag bool
	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "アーカイブで同じ規則を検証する（資金固定・100 株単位・段階手数料）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			start, err := parseDate(sinceFlag)
			if err != nil {
				return err
			}
			end, err := dayOrToday(untilFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			cache := usmarket.DefaultCachePath(appSettings.DataDir)
			var fetcher usmarket.Fetcher
			if cfg.Regime.UsSkipHigh != nil {
				fetcher = usmarket.NewFredFetcher()
			}
			if cfg.Margin.Enabled {
				return runMarginBacktest(cfg, start, end, fetcher, cache, tradesFlag)
			}
			result, err := dtbacktest.Run(openArchive(), cfg, start, end, fetcher, cache)
			if err != nil {
				return err
			}
			printBacktest(cfg, result, start, end, tradesFlag)
			return nil
		},
	}
	cmd.Flags().StringVar(&sinceFlag, "since", "2017-01-01", "開始日")
	cmd.Flags().StringVar(&untilFlag, "until", "", "終了日（既定は最新）")
	cmd.Flags().BoolVar(&tradesFlag, "trades", false, "個別の取引も出す")
	return cmd
}

func printBacktest(cfg dtconfig.Config, result *dtbacktest.Result, start, end time.Time, showTrades bool) {
	s := result.Summary
	fmt.Printf("%s〜%s  資金 %s 円  N=%d  営業日 %d（取引 %d）  往復手数料 %.1f bp\n",
		start.Format(DateLayout), end.Format(DateLayout), yen(s.Capital),
		cfg.Capital.Positions(), s.Days, s.TradedDays, s.RoundTripBP)
	fmt.Printf("損益合計 %s 円  日平均 %s 円  年率 %.1f%%  Sharpe %.2f  最大 DD %s 円  勝率(日) %.1f%%\n",
		yen(s.TotalPnL), yen(s.MeanDaily), s.AnnualReturn*100, s.Sharpe,
		yen(s.MaxDrawdown), s.WinRate*100)
	fmt.Printf("月次: 平均 %s 円  中央値 %s 円  10%% 点 %s 円  勝ち月 %.0f%%  平均銘柄数 %.1f\n",
		yen(s.MonthlyMean), yen(s.MonthlyMedian), yen(s.MonthlyP10),
		s.MonthlyWin*100, s.AvgPositions)

	fmt.Println("\n年別")
	fmt.Printf("  %6s %8s %8s %14s %12s %14s\n", "年", "営業日", "取引日", "損益", "日平均", "勝率(取引日)")
	for _, y := range dtbacktest.YearlyOf(result.Daily) {
		fmt.Printf("  %6d %8d %8d %14s %12s %13.1f%%\n",
			y.Year, y.Days, y.Traded, yen(y.PnL), yen(y.MeanDaily), y.WinRate*100)
	}
	if showTrades {
		printTrades(result.Trades)
	}
}

func runMarginBacktest(cfg dtconfig.Config, start, end time.Time, fetcher usmarket.Fetcher, cache string, showTrades bool) error {
	result, err := dtbacktest.RunMargin(openArchive(), cfg, start, end, fetcher, cache)
	if err != nil {
		return err
	}
	m := cfg.Margin
	cash := m.Cash
	if cash.IsZero() {
		cash = cfg.Capital.MaxCapital
	}
	shrink := "なし"
	if m.LongShrink {
		shrink = "あり"
	}
	fmt.Printf("%s〜%s  現金 %s 円  ロング N=%d %s 円（縮小%s）  ショート N=%d %s 円 × 通常 %s / 弱 %s  営業日 %d\n",
		start.Format(DateLayout), end.Format(DateLayout), yen(cash),
		cfg.Capital.Positions(), yen(cfg.Capital.MaxCapital), shrink,
		m.Positions(), yen(m.MaxCapital), m.MultiplierNormal.String(), m.MultiplierLongWeak.String(),
		result.Summary.Days)

	peak, required := dtbacktest.RequiredMargin(cfg)
	note := fmt.Sprintf("（現金の %.0f%%）", required.Div(cash).InexactFloat64()*100)
	if required.GreaterThan(cash) {
		note = fmt.Sprintf("  現金 %s 円を超えています", yen(cash))
	}
	fmt.Printf("建玉の最大 %s 円 → 保証金 33%% で %s 円%s\n", yen(peak), yen(required), note)

	for _, entry := range []struct {
		label   string
		summary dtbacktest.Summary
	}{
		{"合算", result.Summary},
		{"ロング", result.LongSummary},
		{"ショート", result.ShortSummary},
	} {
		s := entry.summary
		years := float64(s.Days) / dtbacktest.TradingDays
		annual := 0.0
		if years > 0 && !cash.IsZero() {
			annual = s.TotalPnL / years / cash.InexactFloat64()
		}
		fmt.Printf("%-6s 損益 %14s 円  年率(対現金) %6.1f%%  Sharpe %5.2f  最大 DD %12s 円  取引日 %d  往復コスト %.1f bp\n",
			entry.label, yen(s.TotalPnL), annual*100, s.Sharpe, yen(s.MaxDrawdown),
			s.TradedDays, s.RoundTripBP)
	}

	carried, carriedPnL := 0, 0.0
	for _, t := range result.ShortTrades {
		if t.Carried {
			carried++
			carriedPnL += t.PnL
		}
	}
	if len(result.ShortTrades) > 0 {
		fmt.Printf("ショートの張り付き（引けストップ高 → 翌寄りで返済、係数 %s）: %d / %d 件（%.1f%%）  該当の損益 %s 円\n",
			m.CarryPenalty.String(), carried, len(result.ShortTrades),
			float64(carried)/float64(len(result.ShortTrades))*100, yen(carriedPnL))
	}

	fmt.Println("\n年別（ロング / ショート）")
	fmt.Printf("  %6s %8s %8s %14s %14s %14s %14s\n",
		"年", "営業日", "取引日", "合算", "ロング", "ショート", "勝率(取引日)")
	for _, y := range dtbacktest.YearlyOf(result.Daily) {
		fmt.Printf("  %6d %8d %8d %14s %14s %14s %13.1f%%\n",
			y.Year, y.Days, y.Traded, yen(y.PnL), yen(y.LongPnL), yen(y.ShortPnL), y.WinRate*100)
	}
	if showTrades {
		printTrades(result.ShortTrades)
	}
	return nil
}

// printTrades は直近 30 件の取引。全部出すと端末が流れるだけなので末尾に絞る。
func printTrades(trades []dtbacktest.Trade) {
	fmt.Println("\n直近の取引")
	fmt.Printf("  %-11s %-7s %8s %9s %9s %9s %12s\n", "日付", "銘柄", "ギャップ", "株数", "始値", "終値", "損益")
	from := max(0, len(trades)-30)
	for _, t := range trades[from:] {
		fmt.Printf("  %-11s %-7s %8s %9s %9s %9s %12s\n",
			t.Date.Format(DateLayout), t.Code, pct(t.Gap), yen(t.Shares),
			yen(t.Open), yen(t.Close), yen(t.PnL))
	}
}
