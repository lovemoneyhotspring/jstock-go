package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "status",
		Short:        "端点ごとの保存状況（月数・最古・最新・最終取得）",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession("status", false)
			if err != nil {
				return err
			}
			defer s.close()

			tz := clock.MustZone(appSettings.Timezone)
			fmt.Printf("蓄積の状況（%s）\n", jquantsDir)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "端点\t月数\t最古\t最新\t最終取得\t一括")
			for _, ep := range archive.StandardEndpoints {
				// 台帳の件数ではなく Parquet の実体を数える（実データの有無が知りたい）
				months := s.ingestor.Archive.Months(ep)
				oldest, latest := "", ""
				if len(months) > 0 {
					oldest, latest = months[0], months[len(months)-1]
				}
				last := ""
				history, err := s.ledger.History(ep, 1)
				if err != nil {
					return err
				}
				if len(history) > 0 {
					last = clock.Fmt(history[0].FetchedUTC, tz, false)
				}
				bulk := "—"
				if ep.Bulk {
					bulk = "○"
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n",
					ep.Path, len(months), dash(oldest), dash(latest), dash(last), bulk)
			}
			return w.Flush()
		},
	}
}
