package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "設定されている戦略と銘柄の対応表",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t戦略\t基本予算\t対象銘柄\t有効")
			for _, t := range cfg.Tactics {
				en := "○"
				if !t.IsEnabled() {
					en = "×"
				}
				fmt.Fprintf(w, "%s\t%s\t%s円\t%v\t%s\n", t.ID, t.Tactic, t.MonthlyBudget, t.Symbols, en)
			}
			w.Flush()
			return nil
		},
	}
}
