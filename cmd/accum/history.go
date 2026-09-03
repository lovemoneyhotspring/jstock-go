package main

import (
	"os"

	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var from, to, csvPath string
	var limit int
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "history [kind]",
		Short: "積立の判断・評価の履歴（追記専用の Parquet）を一覧・表示・CSV に書き出す",
		Long: "置き場は state/accum/history/<種類>/。台帳（発注したものだけ）と違い、\n" +
			"発注に至らなかった判断も残る。種類を省くと一覧を出す（decision / evaluation）。",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := parseDay(from, "--from")
			if err != nil {
				return jsonAwareError(asJSON, err)
			}
			end, err := parseDay(to, "--to")
			if err != nil {
				return jsonAwareError(asJSON, err)
			}

			kind := ""
			if len(args) > 0 {
				kind = args[0]
			}
			window := history.Range{}
			if start != "" {
				window.Start = mustDay(start)
			}
			if end != "" {
				window.End = mustDay(end)
			}
			return history.Show(os.Stdout, accumhist.StoreFor(appSettings), kind, history.ShowOptions{
				Window:  window,
				Limit:   limit,
				CSVPath: csvPath,
				AsJSON:  asJSON,
			})
		},
	}

	cmd.Flags().StringVar(&from, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().IntVar(&limit, "limit", history.DefaultLimit, "表示する行数")
	cmd.Flags().StringVar(&csvPath, "csv", "", "該当する全行をこのファイルに書き出す")
	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}

// jsonAwareError は --json のときの失敗も JSON で返す。パイプの先が
// 常に 1 個の JSON を読めるようにするため（人向けの文言を混ぜない）。
func jsonAwareError(asJSON bool, err error) error {
	if !asJSON {
		return err
	}
	_ = output.EmitError(err.Error(), "invalid_argument")
	return err
}
