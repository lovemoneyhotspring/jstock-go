package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/spf13/cobra"
)

func newRunsCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "runs",
		Short: "過去の実行を一覧する",
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := repo.OpenRepo(appSettings.DBPath())
			if err != nil {
				return err
			}
			defer r.Close()

			records, err := r.RecentRuns(limit)
			if err != nil {
				return err
			}
			loc := clock.MustZone(appSettings.Timezone)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "run_id\t開始\t基準日\t環境\tモード\t状態\t資産")
			for _, rec := range records {
				equity := "-"
				if rec.Equity != nil {
					equity = rec.Equity.String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					rec.RunID, clock.FmtISO(rec.StartedAt, loc), rec.AsOf,
					rec.Env, rec.Mode, rec.Status, equity)
			}
			w.Flush()
			if len(records) == 0 {
				fmt.Println("実行の記録はまだありません")
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "表示件数")
	return cmd
}
