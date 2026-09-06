package main

import (
	"fmt"
	"strings"
	"time"

	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newSnapCmd() *cobra.Command {
	var symbolsFlag, slotFlag, columnsFlag string
	cmd := &cobra.Command{
		Use:   "snap",
		Short: "板・気配をそのまま履歴に残す（発注しない。docs/OPENING_DATA.md）",
		Long: "時価問合の応答を解釈せずに state/daytrade/history/book/ へ積む。\n" +
			"板は過去に遡れない（J-Quants の分足にもティックにも板は無い）ので、\n" +
			"記録を始めた日からしか手に入らない。選定には使わない——溜めてから、\n" +
			"evaluate の結果と突き合わせて効きを確かめる。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSnap(symbolsFlag, slotFlag, columnsFlag)
		},
	}
	cmd.Flags().StringVar(&symbolsFlag, "symbols", "", "銘柄をカンマ区切りで指定（疎通確認用。既定は plan の全銘柄）")
	cmd.Flags().StringVar(&slotFlag, "slot", "", "観測の時刻帯の名前（既定は今の HHMM）")
	cmd.Flags().StringVar(&columnsFlag, "columns", "", "取りに行く列（既定は book.columns）")
	return cmd
}

func runSnap(symbolsFlag, slotFlag, columnsFlag string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.Book.Enabled {
		fmt.Println("板の記録は無効（book.enabled = false）。何もしません")
		logInfo("daytrade.skip", "板の記録が無効", map[string]any{"reason": "disabled"})
		return nil
	}
	// 板を返せるのは立花の時価問合だけ。csv の気配には板が無い
	if cfg.Execution.QuoteSource != "tachibana" {
		fmt.Printf("quote_source = %q では板を取れません。何もしません\n", cfg.Execution.QuoteSource)
		logInfo("daytrade.skip", "板を取れない取得元",
			map[string]any{"reason": "quote_source", "quote_source": cfg.Execution.QuoteSource})
		return nil
	}

	now := clock.NowUTC()
	day := todayJST(now)
	if skipHoliday(day, "snap") {
		return nil
	}

	symbols, source, err := snapSymbols(cfg.Book.Scope, symbolsFlag, day)
	if err != nil {
		return err
	}
	if len(symbols) == 0 {
		// 平日に plan が無いのは前夜の cron が落ちたとき。板の記録のために
		// 発注系と同じ「無ければ作る」はしない（重い上に、記録は次の回で足りる）
		fmt.Println("対象の銘柄がありません（前夜の plan が走っていない）。何もしません")
		logWarn("daytrade.snap", "対象の銘柄が無い", map[string]any{"day": day.Format(DateLayout)})
		return nil
	}

	slot := slotFlag
	if slot == "" {
		slot = now.In(clock.MustZone(appSettings.Timezone)).Format("1504")
	}
	columns := columnsFlag
	if columns == "" {
		columns = cfg.Book.Columns
	}

	tachibana, err := dtquotes.Connect(appSettings.Env, appSettings.DotenvMap, appSettings.StateDir)
	if err != nil {
		return err
	}
	tachibana.Broker.SetLogger(run)
	// snap は open / close とロックを共有する。遅い日にここで粘ると 9:01 の発注がロックを
	// 取れずに消えるので、book.max_run_seconds で切る（記録は次の回で足りる。発注が優先）
	if cfg.Book.MaxRunSeconds > 0 {
		tachibana.Broker.SetDeadline(now.Add(time.Duration(cfg.Book.MaxRunSeconds) * time.Second))
	}
	// 取れたバッチは積む。失敗したバッチは 1 度取り直され、それでも駄目なら落とす
	// （記録は遡れないので、1 本の失敗で 30 本ぶんを捨てない）
	rows, failed := tachibana.Broker.MarketPricesRawPartial(symbols, columns)
	missing := 0
	for _, f := range failed {
		missing += len(f.Symbols)
	}

	path := ""
	if len(rows) > 0 {
		path = appendHistory(dthistory.KindBook, dthistory.BookFrame(rows, slot, now), day)
	}
	fmt.Printf("%s %s: %d 銘柄に問合せ、%d 行を記録（%s）\n",
		day.Format(DateLayout), slot, len(symbols), len(rows), source)
	if path != "" {
		fmt.Printf("  %s\n", path)
	}
	fields := map[string]any{
		"day": day.Format(DateLayout), "slot": slot, "scope": source,
		"requested": len(symbols), "rows": len(rows), "path": path,
		"elapsed_ms": clock.NowUTC().Sub(now).Milliseconds(), "max_run_seconds": cfg.Book.MaxRunSeconds,
	}
	if len(failed) == 0 {
		logInfo("daytrade.snap", "板を記録", fields)
		return nil
	}
	fields["failed_batches"] = len(failed)
	fields["missing"] = missing
	fields["error"] = failed[0].Error()
	if len(rows) == 0 {
		logError("daytrade.snap", "板の取得に失敗", fields)
	} else {
		logWarn("daytrade.snap", "板の一部を取れなかった（取れた分は記録した）", fields)
	}
	fmt.Printf("  取れなかったバッチ %d/%d（%d 銘柄）: %v\n", len(failed), failed[0].Batches, missing, failed[0])
	return fmt.Errorf("板の一部を取れませんでした（%d/%d バッチ、%d 銘柄）", len(failed), failed[0].Batches, missing)
}

// snapSymbols は記録する銘柄。--symbols があればそれ、無ければ前夜の plan の行。
//
// scope = all は plan の**全行**（除外された銘柄も含む ＝ 実質全上場）。母集団の条件を
// 将来変えたくなったとき、条件の外にあった銘柄の板が無いと検証できない。
func snapSymbols(scope, symbolsFlag string, day time.Time) ([]string, string, error) {
	if symbolsFlag != "" {
		var out []string
		for _, s := range strings.Split(symbolsFlag, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out, "指定", nil
	}
	p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", nil
	}
	onlyUniverse := scope == "universe"
	out := make([]string, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		if onlyUniverse && !c.Eligible && !c.ShortEligible {
			continue
		}
		if c.Symbol == "" {
			continue
		}
		out = append(out, c.Symbol)
	}
	label := "plan の全行"
	if onlyUniverse {
		label = "plan の母集団"
	}
	return out, label, nil
}
