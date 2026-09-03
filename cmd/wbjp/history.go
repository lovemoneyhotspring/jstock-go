package main

import (
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"os"

	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	wbjphistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/history"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var (
		date, start, end, csvPath string
		latest, asJSON            bool
		limit                     int
	)

	cmd := &cobra.Command{
		Use:   "history [kind]",
		Short: "スクリーニングの履歴（追記専用の Parquet）を一覧・表示・CSV に書き出す",
		Long: "スクリーニングの履歴（追記専用の Parquet）を一覧・表示・CSV に書き出す。\n\n" +
			"置き場は state/wbjp/history/<種類>/。1 回の screen が 1 ファイルで上書きしない。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := ""
			if len(args) == 1 {
				kind = args[0]
			}

			// --date は「その日だけ」の省略記法。--from/--to より強い
			day, err := cli.ParseDay(date)
			if err != nil {
				return err
			}
			from, err := cli.ParseDay(start)
			if err != nil {
				return err
			}
			to, err := cli.ParseDay(end)
			if err != nil {
				return err
			}
			if !day.IsZero() {
				from, to = day, day
			}

			return corehistory.Show(os.Stdout, wbjphistory.StoreFor(appSettings), kind, corehistory.ShowOptions{
				Window:     corehistory.Range{Start: from, End: to},
				LatestOnly: latest,
				Limit:      limit,
				CSVPath:    csvPath,
				AsJSON:     asJSON,
			})
		},
	}

	cmd.Flags().StringVar(&date, "date", "", "その基準日だけ YYYY-MM-DD")
	cmd.Flags().StringVar(&start, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&end, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().BoolVar(&latest, "latest", false, "その日（--date か最終日）の最後の実行ぶんだけ")
	cmd.Flags().IntVar(&limit, "limit", corehistory.DefaultLimit, "表示する行数")
	cmd.Flags().StringVar(&csvPath, "csv", "", "該当する全行をこのファイルに書き出す")
	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}
