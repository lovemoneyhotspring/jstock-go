package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dtbacktest "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
)

// runGrid は複数の設定を 1 プロセスで回す（backtest --grid）。
//
// パネルと危険信号の材料は設定に依存しないので 1 回だけ読む。2 本目以降は
// 母集団の当て直しと simulate だけなので数秒で終わる。
func runGrid(dirs []string, start, end time.Time, csvPath, note string,
	fillEntry, fillExit string) error {
	entries := make([]dtbacktest.GridEntry, 0, len(dirs))
	for _, dir := range dirs {
		cfg, err := dtconfig.Load(dir)
		if err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
		entries = append(entries, dtbacktest.GridEntry{Name: filepath.Base(filepath.Clean(dir)), Config: cfg})
	}
	// FRED はどれか 1 つでも要れば用意する（材料は RunGrid が設定ごとに使い回す）
	var fetcher usmarket.Fetcher
	skipOpened := false
	for _, e := range entries {
		if e.Config.Regime.UsSkipHigh != nil {
			fetcher = usmarket.NewFredFetcher()
		}
		skipOpened = skipOpened || e.Config.Signal.SkipOpened
	}
	build := minuteOptionsFor(skipOpened, start, end, fillEntry, fillExit)

	began := time.Now()
	results, err := dtbacktest.RunGrid(openArchive(), entries, start, end, fetcher,
		usmarket.DefaultCachePath(appSettings.DataDir), build)
	if err != nil {
		return err
	}
	printGrid(results, start, end, time.Since(began))
	return writeGridCSV(csvPath, note, results, start, end)
}

func printGrid(results []dtbacktest.GridResult, start, end time.Time, elapsed time.Duration) {
	fmt.Printf("%s〜%s  設定 %d 本  合計 %.1f 秒\n",
		start.Format(DateLayout), end.Format(DateLayout), len(results), elapsed.Seconds())
	fmt.Printf("  %-22s %14s %8s %7s %14s %8s %9s %8s\n",
		"設定", "損益", "年率", "Sharpe", "最大 DD", "取引日", "張り付き", "所要")
	for _, g := range results {
		s := g.Summary()
		years := float64(s.Days) / dtbacktest.TradingDays
		annual := 0.0
		if years > 0 && g.Capital() > 0 {
			annual = s.TotalPnL / years / g.Capital()
		}
		pinned := "—"
		if carried, total := g.Carried(); total > 0 {
			pinned = fmt.Sprintf("%d/%d", carried, total)
		}
		fmt.Printf("  %-22s %14s %7.1f%% %7.2f %14s %8d %9s %7.1fs\n",
			g.Name, yen(s.TotalPnL), annual*100, s.Sharpe, yen(s.MaxDrawdown),
			s.TradedDays, pinned, g.Elapsed.Seconds())
	}
	var preopen []string
	for _, g := range results {
		if g.Config.Execution.PreopenEnabled() {
			preopen = append(preopen, g.Name+preopenTag(g.Config))
		}
	}
	if len(preopen) > 0 {
		fmt.Printf("注意: %s は寄成で**始値ちょうど**に建つ想定で、寄付のスプレッドを払っていません"+
			"（--csv の subject にも同じ印が付きます）\n", strings.Join(preopen, " / "))
	}
}

// preopenTag は結果に付ける「寄る前に寄成で建てる想定」の印（none なら空）。
//
// **20-research/結果.csv の subject に混ぜる。** execution.preopen_legs を none 以外にすると、
// その脚は検証の全期間で寄付のスプレッドを払わなくなる（backtest.legParams.spreadBP）ので、
// none で測った過去の行と同じ subject に並べると物差しの違いが消える。印を付けて
// 「同じ名前の別物」が 1 つの subject に混ざらないようにする。
func preopenTag(cfg dtconfig.Config) string {
	if !cfg.Execution.PreopenEnabled() {
		return ""
	}
	return "（寄成 " + cfg.Execution.PreopenLegs + "）"
}

// writeGridCSV は 20-research/結果.csv と同じ列で書く（そのまま追記できる形）。
func writeGridCSV(path, note string, results []dtbacktest.GridResult, start, end time.Time) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"note", "system", "market", "kind", "subject", "period", "metric", "value", "unit", "role"})
	period := fmt.Sprintf("%s〜%s", start.Format("2006-01"), end.Format("2006-01"))
	for _, g := range results {
		s := g.Summary()
		// 寄成で建てる想定の行は subject に印を付ける（preopenTag。物差しが違う）
		subject := g.Name + preopenTag(g.Config)
		years := float64(s.Days) / dtbacktest.TradingDays
		annual := 0.0
		if years > 0 && g.Capital() > 0 {
			annual = s.TotalPnL / years / g.Capital() * 100
		}
		dd := 0.0
		if g.Capital() > 0 {
			dd = -s.MaxDrawdown / g.Capital() * 100
		}
		for _, m := range []struct {
			metric string
			value  float64
			unit   string
		}{
			{"total_pnl", s.TotalPnL, "yen"},
			{"annual_return", annual, "pct"},
			{"sharpe", s.Sharpe, "ratio"},
			{"max_dd", dd, "pct"},
			{"traded_days", float64(s.TradedDays), "days"},
		} {
			_ = w.Write([]string{note, "daytrade", "日本株", "backtest", subject, period,
				m.metric, strconv.FormatFloat(m.value, 'f', -1, 64), m.unit, "候補"})
		}
	}
	w.Flush()
	return w.Error()
}
