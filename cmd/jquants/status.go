package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "端点ごとの蓄積状況を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			arch, err := archive.OpenArchive(jquantsDir)
			if err != nil {
				return err
			}
			defer arch.Close()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "端点\t種別\t最新データ\t件数\t更新日時")

			for _, ep := range archive.StandardEndpoints {
				kind := "日次"
				if ep.Bulk {
					kind = "月次/一括"
				}
				updated, latest, count, _ := arch.EndpointStatus(ep.Name)
				if count == 0 {
					latest = "-"
					updated = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", ep.Name, kind, latest, count, updated)
			}
			w.Flush()
			return nil
		},
	}
}
