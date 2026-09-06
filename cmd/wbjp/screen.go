package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	wbjphistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newScreenCmd() *cobra.Command {
	var showFailed bool
	var noSave bool
	var asJSON bool
	var top int

	cmd := &cobra.Command{
		Use:   "screen",
		Short: "戦略の合致度による銘柄順位表を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
			if err != nil {
				return err
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			strats, weights, err := buildStrategies(stratCfg)
			if err != nil {
				return err
			}
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			allBars := make(map[string][]domain.Bar)
			for _, sym := range setCfg.Universe.Symbols {
				bars, err := barStore.Read(sym, "", "")
				if err == nil && len(bars) > 0 {
					allBars[sym] = bars
				}
			}

			today := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02")
			// 建玉は渡さない。screen は「今から建てるなら何か」を見るための
			// コマンドなので、保有の有無で結果が変わらないほうが読みやすい。
			screenUniverse := strategy.NewUniverse(allBars)
			if needsMargin(stratCfg) {
				book, err := loadMarginBook(setCfg.Universe.Symbols)
				if err != nil {
					return err
				}
				screenUniverse.SetMargin(book)
			}
			ctx := screenUniverse.At(today, nil, decimal.Zero)

			signalsBySymbol := make(map[string][]domain.Signal)
			for _, s := range strats {
				sigs, err := s.OnBars(ctx)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s の評価に失敗: %v\n", s.Name(), err)
					continue
				}
				for _, sig := range sigs {
					signalsBySymbol[sig.Symbol] = append(signalsBySymbol[sig.Symbol], sig)
				}
			}

			var combinedAll []domain.CombinedSignal
			for _, sym := range setCfg.Universe.Symbols {
				if !ctx.HasBars(sym, 1) {
					continue
				}
				combinedAll = append(combinedAll, combineFunc(sym, signalsBySymbol[sym], weights))
			}

			sort.Slice(combinedAll, func(i, j int) bool {
				if combinedAll[i].Direction != combinedAll[j].Direction {
					return combinedAll[i].Direction > combinedAll[j].Direction
				}
				return combinedAll[i].Symbol < combinedAll[j].Symbol
			})

			// 順位表を履歴に残す。評価（evaluate / review）はこれを材料に、
			// 「その日の判断が当たっていたか」を後から突き合わせる。表示を
			// 絞る --top とは独立に、全銘柄を残す。
			if !noSave {
				frame := wbjphistory.ScreenFrame(combinedAll, wbjphistory.ScreenOptions{
					Threshold:    stratCfg.EntryThreshold,
					MaxPositions: setCfg.Sizing.MaxPositions,
					Combiner:     stratCfg.Combiner,
					Close: func(symbol string) decimal.Decimal {
						bars := allBars[symbol]
						if len(bars) == 0 {
							return decimal.Zero
						}
						return bars[len(bars)-1].Close
					},
				})
				store := wbjphistory.StoreFor(appSettings)
				if _, err := store.Append(wbjphistory.Kind, frame, clock.NowUTC(), corehistory.AppendOptions{}); err != nil {
					fmt.Fprintf(os.Stderr, "順位表の履歴を残せませんでした: %v\n", err)
				}
			}

			shown := combinedAll
			if top > 0 && top < len(shown) {
				shown = shown[:top]
			}

			if asJSON {
				rows := make([]map[string]any, 0, len(shown))
				for i, item := range shown {
					rows = append(rows, map[string]any{
						"rank":      i + 1,
						"symbol":    item.Symbol,
						"direction": item.Direction,
						"reason":    item.Reason,
						"passed":    item.Direction >= stratCfg.EntryThreshold,
					})
				}
				return output.EmitJSON(map[string]any{"as_of": today, "items": rows})
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "順位\t銘柄\t合成スコア\t判定")
			for i, item := range shown {
				judgment := "中立"
				if item.Direction >= stratCfg.EntryThreshold {
					judgment = "★ 買い建て候補"
				} else if item.Direction <= -stratCfg.ExitThreshold {
					judgment = "▼ 手仕舞い候補"
				}
				fmt.Fprintf(w, "%d\t%s\t%.2f\t%s\n", i+1, item.Symbol, item.Direction, judgment)
			}
			w.Flush()

			if showFailed {
				printFailedReasons(strats, ctx, setCfg.Universe.Symbols)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&showFailed, "show-failed", false, "各戦略が銘柄を落とした理由を表示する")
	cmd.Flags().BoolVar(&noSave, "no-save", false, "順位表を履歴に残さない")
	cmd.Flags().BoolVar(&asJSON, "json", false, "JSON で出力する")
	cmd.Flags().IntVar(&top, "top", 0, "表示する上位件数（0 で全件）")
	return cmd
}

// printFailedReasons は「なぜ候補にならなかったか」を戦略ごとに並べる。
//
// 候補が 0 件のとき、条件が厳しすぎるのか、データが足りないのか、地合いで
// 止まっているのかを切り分けられないと調整のしようがない。
func printFailedReasons(strats []strategy.Strategy, ctx *strategy.Context, symbols []string) {
	for _, s := range strats {
		screener, ok := s.(strategy.Screener)
		if !ok {
			continue
		}
		fmt.Printf("\n=== %s の不合格理由 ===\n", s.Describe())
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, sym := range symbols {
			if !ctx.HasBars(sym, 1) {
				continue
			}
			res := screener.Screen(ctx, sym)
			if res.Passed() {
				fmt.Fprintf(w, "%s\t合格 (スコア %.2f)\n", sym, res.Score)
				continue
			}
			fmt.Fprintf(w, "%s\t%s\n", sym, strings.Join(res.Failed, " / "))
		}
		w.Flush()
	}
}
