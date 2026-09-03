package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/spf13/cobra"
)

func newStopsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stops",
		Short: "ストップロスの現在状況を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := repo.OpenRepo(appSettings.DBPath())
			if err != nil {
				return err
			}
			defer rep.Close()

			stopMap, err := rep.GetStops()
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "銘柄\tストップ価格\t建値\tATR倍率\t最高終値\t作成日")
			for sym, st := range stopMap {
				hc := "-"
				if st.HighestClose != nil {
					hc = st.HighestClose.String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					sym, st.StopPrice, st.EntryPrice, st.ATRMultiple, hc, st.CreatedOn)
			}
			w.Flush()
			return nil
		},
	}
}
