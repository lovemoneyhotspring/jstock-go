package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/evaluate"
	wbjphistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/history"
	"github.com/spf13/cobra"
)

func newReviewCmd() *cobra.Command {
	var (
		start, end string
		asJSON     bool
	)

	cmd := &cobra.Command{
		Use:   "review",
		Short: "選定の妥当性を日ごとに見る。採用・次点・圏外の平均リターンを並べる",
		Long: "選定の妥当性を日ごとに見る。採用・次点・圏外の平均リターンを並べる。\n\n" +
			"材料は wbjp evaluate が積んだ履歴。",
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := parseDay(start)
			if err != nil {
				return err
			}
			to, err := parseDay(end)
			if err != nil {
				return err
			}

			store := wbjphistory.StoreFor(appSettings)
			frame, err := store.Read(evaluate.Kind, corehistory.Range{Start: from, End: to})
			if err != nil {
				return err
			}
			table := evaluate.Review(frame)
			totals := evaluate.ReviewTotals(table)

			if asJSON {
				return output.EmitJSON(map[string]any{
					"ok":     true,
					"rows":   output.RowsOf(table),
					"totals": output.RowsOf(totals),
				})
			}
			if table.Height() == 0 {
				fmt.Println("評価の履歴がありません（wbjp evaluate を回す）")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "判断日\t日数\t採用\tadopted bp\tpassed bp\trest bp")
			for _, row := range table.Rows {
				fmt.Fprintf(w, "%s\t%v\t%v\t%s\t%s\t%s\n",
					corehistory.Cell(row["day"]), row["horizon"], row["adopted"],
					signedBP(row["adopted_bp"]), signedBP(row["passed_bp"]), signedBP(row["rest_bp"]))
			}
			w.Flush()

			for _, row := range totals.Rows {
				fmt.Printf("\n合計  %v 日  採用 %s bp / 圏外 %s bp  採用が上回った日 %s\n",
					row["days"], signedBP(row["avg_adopted_bp"]), signedBP(row["avg_rest_bp"]),
					percent(row["beat_rest_rate"]))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&start, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&end, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}
