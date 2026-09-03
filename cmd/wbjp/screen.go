package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/spf13/cobra"
)

func newScreenCmd() *cobra.Command {
	return &cobra.Command{
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
			strats, weights := buildStrategies(stratCfg)
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			type screenItem struct {
				symbol    string
				direction float64
				reason    string
			}
			var items []screenItem

			for _, sym := range setCfg.Universe.Symbols {
				bars, err := barStore.Read(sym, "", "")
				if err != nil || len(bars) == 0 {
					continue
				}

				var sigs []domain.Signal
				for _, s := range strats {
					sig, err := s.OnBars(sym, bars)
					if err == nil && sig != nil {
						sigs = append(sigs, *sig)
					}
				}

				combined := combineFunc(sym, sigs, weights)
				items = append(items, screenItem{
					symbol:    sym,
					direction: combined.Direction,
					reason:    combined.Reason,
				})
			}

			sort.Slice(items, func(i, j int) bool {
				return items[i].direction > items[j].direction
			})

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "順位\t銘柄\t合成スコア\t判定")
			for i, item := range items {
				judgment := "中立"
				if item.direction >= stratCfg.EntryThreshold {
					judgment = "★ 買い建て候補"
				} else if item.direction <= -stratCfg.ExitThreshold {
					judgment = "▼ 手仕舞い候補"
				}
				fmt.Fprintf(w, "%d\t%s\t%.2f\t%s\n", i+1, item.symbol, item.direction, judgment)
			}
			w.Flush()
			return nil
		},
	}
}
