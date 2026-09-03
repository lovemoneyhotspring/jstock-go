package main

import (
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var (
		dateFlag, fromFlag, toFlag, csvFlag string
		latestFlag, jsonFlag                bool
		limitFlag                           int
	)
	cmd := &cobra.Command{
		Use:   "history [kind]",
		Short: "選定の履歴（追記専用の Parquet）を一覧・表示・CSV に書き出す",
		Long: "選定の履歴（追記専用の Parquet）を一覧・表示・CSV に書き出す。\n\n" +
			"置き場は state/daytrade/history/<種類>/。1 回の plan / open が 1 ファイルで、\n" +
			"再試行や dry-run の確認も全部残る。DuckDB から直接 read_parquet で読める。\n" +
			"種類: plan / plan_meta / quotes / ranking / open_run / evaluation / execution",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := ""
			if len(args) == 1 {
				kind = args[0]
			}
			day, err := parseDate(dateFlag)
			if err != nil {
				return err
			}
			window := history.Range{}
			if !day.IsZero() {
				window.Start, window.End = day, day
			} else {
				if window.Start, err = parseDate(fromFlag); err != nil {
					return err
				}
				if window.End, err = parseDate(toFlag); err != nil {
					return err
				}
			}
			return history.Show(os.Stdout, historyStore(), kind, history.ShowOptions{
				Window: window, LatestOnly: latestFlag, Limit: limitFlag,
				CSVPath: csvFlag, AsJSON: jsonFlag,
			})
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "その日だけ（YYYY-MM-DD）")
	cmd.Flags().StringVar(&fromFlag, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&toFlag, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().BoolVar(&latestFlag, "latest", false, "その日の最後の実行ぶんだけ")
	cmd.Flags().IntVar(&limitFlag, "limit", history.DefaultLimit, "表示する行数")
	cmd.Flags().StringVar(&csvFlag, "csv", "", "該当する全行をこのファイルに書き出す")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}
