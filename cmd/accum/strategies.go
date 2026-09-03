package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newStrategiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "strategies",
		Short: "使える戦略（タクティクス）の一覧",
		Run: func(cmd *cobra.Command, args []string) {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "名前\t説明")
			fmt.Fprintln(w, "constant\t定額。常に1倍。純粋なドル平均法")
			fmt.Fprintln(w, "bear_stack\t完全下降配列（終値 < MA20 < MA50 < MA200）で増額")
			fmt.Fprintln(w, "stack_ladder\t弱気スコア（0〜6）に応じて段階的に増額")
			fmt.Fprintln(w, "drawdown_ladder\t過去最高値からの下落率に応じて段階的に増額")
			w.Flush()
		},
	}
}
