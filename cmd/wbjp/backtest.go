package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/engine"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newBacktestCmd() *cobra.Command {
	var fromFlag, toFlag, fillModelFlag string
	var cashFlag int64
	var marginLagFlag int

	cmd := &cobra.Command{
		Use:   "backtest",
		Short: "過去データを用いてスイング戦略のバックテストを実行する",
		Long: "保存済みの足でバックテストする。\n\n" +
			"判断ロジックは約定モデルによらず同一。--fill-model intrabar は指値を\n" +
			"その足の高安で約定させる第 2 の見立てで、既定（open）との突き合わせに使う。",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !engine.ValidFillModel(fillModelFlag) {
				return fmt.Errorf("--fill-model は %s: %s",
					strings.Join(engine.FillModels, " / "), fillModelFlag)
			}

			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
			if err != nil {
				return err
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			allBars := make(map[string][]domain.Bar)
			for _, sym := range setCfg.Universe.Symbols {
				// 期間で切らずに全部読む。--from より前の足は売買の対象外だが、
				// 指標を確定させるウォームアップとして戦略に見せる必要がある。
				bars, err := barStore.Read(sym, "", "")
				if err == nil && len(bars) > 0 {
					allBars[sym] = bars
				}
			}

			if len(allBars) == 0 {
				return fmt.Errorf("バックテスト用の足データがありません。先に 'wbjp data sync' を実行してください")
			}

			strats, weights, err := buildStrategies(stratCfg)
			if err != nil {
				return err
			}
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			// 初期資金の既定は市場に合わせる（円 300 万 / ドル 3 万）。
			// 単元 100 株の日本株を数銘柄持つには 100 万では足りない。
			cash := decimal.NewFromInt(cashFlag)
			if cashFlag <= 0 {
				if setCfg.Universe.Market == string(domain.MarketUS) {
					cash = decimal.NewFromInt(30_000)
				} else {
					cash = decimal.NewFromInt(3_000_000)
				}
			}

			opts := engine.BacktestOptions{
				Start:     fromFlag,
				End:       toFlag,
				FillModel: fillModelFlag,
			}

			// 信用残は使う戦略があるときだけ読む（200 万行の走査）。
			if needsMargin(stratCfg) {
				book, err := loadMarginBookWithLag(setCfg.Universe.Symbols, marginLagFlag)
				if err != nil {
					return err
				}
				if book == nil {
					fmt.Println("信用残（markets/margin-interest）がアーカイブにありません。margin_balance は黙ります")
				} else {
					fmt.Printf("信用残: %d 銘柄 %d 週\n\n", book.Len(), book.Weeks())
				}
				opts.Margin = book
			}

			// 待機資金の利回り。足が無ければ無利息で続ける——
			// 利回りの系列が揃っていないだけで検証全体を止める理由はない。
			if symbol := setCfg.Regime.CashYieldSymbol; symbol != "" {
				yieldBars, err := barStore.Read(symbol, "", "")
				if err != nil || len(yieldBars) == 0 {
					fmt.Printf("待機資金の利回り %s の足がありません。無利息で続けます\n\n", symbol)
				} else {
					opts.CashYield = yieldBars
				}
			}

			stats, err := engine.RunBacktest(setCfg, stratCfg, strats, weights,
				combineFunc, allBars, cash, opts)
			if err != nil {
				return err
			}

			printBacktestStats(stats, fillModelFlag)

			if fillModelFlag == "intrabar" && setCfg.Execution.OrderType == "limit" {
				fmt.Println("\n※ intrabar は指値をその足の高安で約定させるため、" +
					"既定の open（寄付だけで判定）より約定しやすく、成績は楽観的に出ます")
			}
			fmt.Println("\n※ 過去の成績は将来を保証しません。" +
				"少数銘柄・短期間の結果は特に当てになりません")
			return nil
		},
	}

	cmd.Flags().StringVar(&fromFlag, "from", "2024-01-01", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&toFlag, "to", "", "終了日 YYYY-MM-DD（既定は最終営業日）")
	cmd.Flags().Int64Var(&cashFlag, "cash", 0, "初期資金（口座通貨。既定は円300万/ドル3万）")
	cmd.Flags().IntVar(&marginLagFlag, "margin-lag-days", strategy.MarginPublicationLag,
		"信用残が見えるまでの遅れ（暦日）。実際より長くして情報の鮮度を検定する")
	cmd.Flags().StringVar(&fillModelFlag, "fill-model", "open",
		"指値の約定判定: open（寄付だけ。保守的）/ intrabar（高安も見る。楽観的）")
	return cmd
}

func printBacktestStats(stats *engine.BacktestStats, fillModel string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "=== バックテスト結果（約定モデル: %s） ===\n", fillModel)
	fmt.Fprintf(w, "日数\t%d\n", stats.Days)
	fmt.Fprintf(w, "初期資産\t%s 円\n", stats.InitialEquity.StringFixed(0))
	fmt.Fprintf(w, "最終資産\t%s 円\n", stats.FinalEquity.Round(0))
	fmt.Fprintf(w, "総リターン\t%.2f%%\n", stats.TotalReturn.Mul(decimal.NewFromInt(100)).InexactFloat64())
	fmt.Fprintf(w, "最大ドローダウン\t%.2f%%\n", stats.MaxDrawdown.Mul(decimal.NewFromInt(100)).InexactFloat64())
	fmt.Fprintf(w, "約定回数\t%d\n", stats.TotalFills)
	fmt.Fprintf(w, "決済回数\t%d\n", stats.SellFills)
	if stats.Interest.IsPositive() {
		fmt.Fprintf(w, "待機資金の利息\t%s 円\n", stats.Interest.Round(0))
	}

	keys := make([]string, 0, len(stats.Analysis))
	for k := range stats.Analysis {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s\t%s\n", k, stats.Analysis[k])
	}
	w.Flush()
}
