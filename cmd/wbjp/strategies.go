package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/spf13/cobra"
)

func newStrategiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "strategies",
		Short: "使える戦略（ストラテジー）の一覧",
		Run: func(cmd *cobra.Command, args []string) {
			// 一覧は登録簿から作る。ここに書き足す必要はない。
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "名前\t説明")
			for _, d := range strategy.Describe() {
				fmt.Fprintf(w, "%s\t%s\n", d.Name, d.Summary)
			}
			w.Flush()
		},
	}
}
