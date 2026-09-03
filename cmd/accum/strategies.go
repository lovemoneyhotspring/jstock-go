package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/spf13/cobra"
)

func newStrategiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "strategies",
		Short: "使える戦略（タクティクス）の一覧",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "名前\t既定パラメータ\t発注時間帯\t説明")
			for _, name := range tactics.Available() {
				t, err := tactics.Create(name)
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					name, t.Describe(), t.Window().Describe(), tactics.Summary(name))
			}
			return w.Flush()
		},
	}
}
