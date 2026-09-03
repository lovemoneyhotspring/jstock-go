package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newOrdersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "orders",
		Short: "積立の発注台帳を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			led, err := ledger.OpenLedger(appSettings.AccumDBPath())
			if err != nil {
				return err
			}
			defer led.Close()

			orders, err := led.Recent(20)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "日時\t銘柄\t数量\t約定数\t状態\t対象月\t注文ID")
			for _, o := range orders {
				pm := "-"
				if o.PlanMonth != nil {
					pm = *o.PlanMonth
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					clock.FmtISO(o.PlacedAt, clock.Tokyo), o.Symbol, o.Quantity, o.FilledQuantity, o.Status, pm, o.ClientOrderID)
			}
			w.Flush()
			return nil
		},
	}
}
