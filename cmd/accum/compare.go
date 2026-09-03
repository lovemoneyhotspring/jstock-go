package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/simulate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newCompareCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "compare [symbol]",
		Short: "1銘柄に対して、登録済みの戦略を既定パラメータで並べて比較する",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			symbol := args[0]
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return jsonAwareError(asJSON, err)
			}
			barStore := data.NewBarStore(appSettings.BarsDir())
			bars, err := barStore.Read(symbol, "", "")
			if err != nil || len(bars) == 0 {
				return jsonAwareError(asJSON,
					fmt.Errorf("%s の足データがありません。先に 'accum sync' を実行してください", symbol))
			}

			// 登録簿の全戦略を既定パラメータで並べる。
			availableTactics, err := tactics.CreateAll()
			if err != nil {
				return jsonAwareError(asJSON, err)
			}

			rows := make([]map[string]any, 0, len(availableTactics))
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
				// 追加資金1倍あたりの単価改善。効果を資金量で割った資金効率の指標
				extra := res.CapitalMultiple - 1.0
				var efficiency any
				if extra > 0.001 {
					efficiency = res.CostEdge / extra
				}
				rows = append(rows, map[string]any{
					"tactic":           tac.Name(),
					"describe":         tac.Describe(),
					"capital_multiple": res.CapitalMultiple,
					"average_cost":     res.AverageCost,
					"cost_edge":        res.CostEdge,
					"edge_per_capital": efficiency,
					"terminal_value":   res.TerminalValue,
				})
			}

			if asJSON {
				return output.EmitJSON(map[string]any{
					"ok": true, "symbol": symbol, "rows": rows,
				})
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "=== %s — 戦略の比較 ===\n", symbol)
			fmt.Fprintln(w, "戦略\t倍率\t単価\t対照比\t1倍あたり\t期末")
			for _, r := range rows {
				efficiency := "—"
				if v, ok := r["edge_per_capital"].(float64); ok {
					efficiency = fmt.Sprintf("%+.1f%%", v*100)
				}
				fmt.Fprintf(w, "%s\t%.2f\t%s\t%+.2f%%\t%s\t%s\n",
					r["describe"], r["capital_multiple"], yen(r["average_cost"].(float64)),
					r["cost_edge"].(float64)*100, efficiency, yen(r["terminal_value"].(float64)))
			}
			w.Flush()

			fmt.Printf("\n※ 既定パラメータでの比較。倍率などは %s で調整する。\n",
				filepath.Join(configDirFlag, "accum.toml"))
			fmt.Println("　 「1倍あたり」は追加資金1倍あたりの単価改善で、資金効率の指標。")
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}
