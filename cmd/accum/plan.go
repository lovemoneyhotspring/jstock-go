package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/spf13/cobra"
)

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "直近の投下計画を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "日付\t銘柄\t終値\t倍率\t基本\t増額\t投下額\t理由")

			for _, entry := range cfg.Tactics {
				if !entry.IsEnabled() {
					continue
				}
				var tactic tactics.Tactic = &tactics.Constant{}
				switch entry.Tactic {
				case "bear_stack":
					tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
				case "stack_ladder":
					tactic = tactics.NewStackLadder(nil, entry.Fast, entry.Mid, entry.Slow)
				case "drawdown_ladder":
					tactic = tactics.NewDrawdownLadder(nil, nil, true, 200)
				}

				var signalBars []domain.Bar
				if entry.SignalSymbol != "" {
					signalBars, _ = barStore.Read(entry.SignalSymbol, "", "")
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, "", "")
					if err != nil || len(bars) == 0 {
						continue
					}
					p, err := plan.BuildPlanWithSignal(bars, signalBars, false, tactic, entry.MonthlyBudget)
					if err != nil || len(p.Rows) == 0 {
						continue
					}
					// 直近5日分を表示
					start := len(p.Rows) - 5
					if start < 0 {
						start = 0
					}
					for i := start; i < len(p.Rows); i++ {
						r := p.Rows[i]
						fmt.Fprintf(w, "%s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%s\n",
							r.Date, sym, r.Close, r.Multiplier, r.Base, r.Extra, r.Amount, r.Reason)
					}
				}
			}
			w.Flush()
			return nil
		},
	}
}
