package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/simulate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/spf13/cobra"
)

func newCompareCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "compare [symbol]",
		Short: "1銘柄に対して登録された全戦略を並べてシミュレーション比較する",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			symbol := args[0]
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())
			bars, err := barStore.Read(symbol, "", "")
			if err != nil || len(bars) == 0 {
				return fmt.Errorf("%s の足データがありません。先に 'accum sync' を実行してください", symbol)
			}

			availableTactics := []tactics.Tactic{
				&tactics.Constant{},
				tactics.NewBearStack(4.0, 20, 50, 200),
				tactics.NewBearStack(2.0, 20, 50, 200),
				tactics.NewStackLadder(nil, 20, 50, 200),
				tactics.NewDrawdownLadder(nil, nil, true, 200),
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "=== %s 戦略比較 ===\n", symbol)
			fmt.Fprintln(w, "戦略\t資金倍率\t平均単価\t対照比\t効率\t期末評価額")

			for _, tac := range availableTactics {
				if len(bars) < tac.WarmupBars() {
					continue
				}
				p, err := plan.BuildPlan(bars, tac, cfg.MonthlyBudget)
				if err != nil || len(p.Rows) == 0 {
					continue
				}

				res, err := simulate.Simulate(bars, p, cfg.MonthlyBudget)
				if err != nil {
					continue
				}

				extra := res.CapitalMultiple - 1.0
				efficiency := "—"
				if extra > 0.001 {
					efficiency = fmt.Sprintf("%+.1f%%", (res.CostEdge/extra)*100)
				}

				fmt.Fprintf(w, "%s\t%.2f倍\t%.2f円\t%+.2f%%\t%s\t%.0f円\n",
					tac.Describe(), res.CapitalMultiple, res.AverageCost, res.CostEdge*100, efficiency, res.TerminalValue)
			}
			w.Flush()
			return nil
		},
	}
}
