package main

import (
	"fmt"
	"time"

	dtevaluate "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newReviewCmd() *cobra.Command {
	var fromFlag, toFlag, csvFlag string
	var daysFlag int
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "review",
		Short: "選定の妥当性を日ごとに見る（選んだ N・次点・候補全体の平均 net bp と損益）",
		Long: "選定の妥当性を日ごとに見る。選んだ N・次点・候補全体の平均 net bp と損益を並べる。\n\n" +
			"選定が効いていれば picked ≥ next ≥ all の日が多い。逆が続けば、順位付けの規則が\n" +
			"その相場で効いていない合図。材料は daytrade evaluate が積んだ履歴。",
		RunE: func(cmd *cobra.Command, args []string) error {
			store := historyStore()
			first, err := parseDate(fromFlag)
			if err != nil {
				return err
			}
			last, err := parseDate(toFlag)
			if err != nil {
				return err
			}
			if first.IsZero() && last.IsZero() {
				known := store.Days(dthistory.KindEvaluation)
				if len(known) == 0 {
					if jsonFlag {
						return output.EmitJSON(map[string]any{"ok": true, "rows": []any{}, "totals": []any{}})
					}
					fmt.Println("評価の履歴がまだありません（daytrade evaluate を回す）")
					return nil
				}
				first = known[0]
				if len(known) >= daysFlag {
					first = known[len(known)-daysFlag]
				}
			}
			evaluations, err := store.Read(dthistory.KindEvaluation, history.Range{Start: first, End: last})
			if err != nil {
				return err
			}
			table := dtevaluate.Review(evaluations)
			totals := dtevaluate.ReviewTotals(table)

			if jsonFlag {
				return output.EmitJSON(map[string]any{
					"ok": true, "from": dayText(first), "to": dayText(last),
					"rows": output.RowsOf(table), "totals": output.RowsOf(totals),
				})
			}
			if table.Height() == 0 {
				fmt.Println("期間内に評価の履歴がありません")
				return nil
			}
			if csvFlag != "" {
				if err := history.WriteCSV(csvFlag, table); err != nil {
					return err
				}
				fmt.Printf("%d 行を %s に書き出しました\n", table.Height(), csvFlag)
			}
			printReview(table, totals)
			return nil
		},
	}
	cmd.Flags().StringVar(&fromFlag, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&toFlag, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().IntVar(&daysFlag, "days", 20, "期間を省いたとき、直近の評価日数")
	cmd.Flags().StringVar(&csvFlag, "csv", "", "日別の表をこのファイルに書き出す")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}

func printReview(table, totals history.Frame) {
	fmt.Println("日別（net bp は費用後の平均。想定損益は選んだ N を建てていた場合）")
	fmt.Printf("  %-11s %-5s %-7s %4s %10s %10s %10s %12s %6s %12s %6s\n",
		"日付", "脚", "順位表", "N", "picked bp", "next bp", "all bp", "想定損益", "建てた", "実現損益", "候補")
	for _, row := range table.Rows {
		source := "始値"
		if strOf(row["source"]) == dtevaluate.SourceQuotes {
			source = "気配"
		}
		day := ""
		if t, ok := row["day"].(time.Time); ok {
			day = t.Format(DateLayout)
		}
		fmt.Printf("  %-11s %-5s %-7s %4d %10s %10s %10s %12s %6d %12s %6d\n",
			day, strOf(row["side"]), source, iOf(row["picked_n"]),
			bpText(row["picked_bp"]), bpText(row["next_bp"]), bpText(row["all_bp"]),
			pnlText(row["picked_pnl"]), iOf(row["traded"]), pnlText(row["actual_pnl"]),
			iOf(row["candidates"]))
	}
	fmt.Println("\n期間の合計（bp は日別平均の平均）")
	fmt.Printf("  %-5s %6s %10s %10s %10s %18s %26s %14s %14s\n",
		"脚", "日数", "picked bp", "next bp", "all bp",
		"picked が勝った日", "picked が all を上回った日", "想定損益", "実現損益")
	for _, row := range totals.Rows {
		fmt.Printf("  %-5s %6d %10s %10s %10s %18s %26s %14s %14s\n",
			strOf(row["side"]), iOf(row["days"]),
			bpText(row["picked_bp"]), bpText(row["next_bp"]), bpText(row["all_bp"]),
			rateText(row["picked_win_days"]), rateText(row["beat_all_days"]),
			pnlText(row["picked_pnl"]), pnlText(row["actual_pnl"]))
	}
}

func dayText(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Format(DateLayout)
}
