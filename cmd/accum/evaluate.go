package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/evaluate"
	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/spf13/cobra"
)

func newEvaluateCmd() *cobra.Command {
	var horizon int
	var from, to string
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "積立の判断に後日の足を当て、増額した日は安かったかを倍率の帯ごとに見る",
		Long: "倍率 1.0（通常の積立）が対照群。増額の帯がそれより高いリターンなら、\n" +
			"下落局面で増やす規則は効いている。材料は accum run が積んだ判断履歴。",
		RunE: func(cmd *cobra.Command, args []string) error {
			start, err := parseDay(from, "--from")
			if err != nil {
				return jsonAwareError(asJSON, err)
			}
			end, err := parseDay(to, "--to")
			if err != nil {
				return jsonAwareError(asJSON, err)
			}

			store := accumhist.StoreFor(appSettings)
			window := history.Range{}
			if start != "" {
				window.Start = mustDay(start)
			}
			if end != "" {
				window.End = mustDay(end)
			}
			decisions, err := store.Read(accumhist.Kind, window)
			if err != nil {
				return jsonAwareError(asJSON, err)
			}
			if decisions.Height() == 0 {
				message := "判断の履歴がありません（accum run を回す）"
				if asJSON {
					return output.EmitJSON(map[string]any{
						"ok": true, "rows": []any{}, "summary": []any{}, "message": message,
					})
				}
				fmt.Println(message)
				return nil
			}

			result := evaluate.Evaluate(decisions, data.NewBarStore(appSettings.BarsDir()), horizon)
			summary := evaluate.Summarize(result)
			scored := evaluate.Scored(result)

			// 実績が 1 件も無いうちは積まない。評価は後から何度でも作り直せるので、
			// 中身の無いファイルを残す意味がない
			path := ""
			if scored.Height() > 0 {
				path, err = store.Append(evaluate.Kind, result, clock.TodayUTC(), history.AppendOptions{Ctx: runCtx})
				if err != nil {
					return jsonAwareError(asJSON, err)
				}
			}
			digest.Note(map[string]any{
				"rows": result.Height(), "scored": scored.Height(), "horizon": horizon,
			})

			if asJSON {
				return output.EmitJSON(map[string]any{
					"ok":      true,
					"horizon": horizon,
					"path":    path,
					"summary": output.RowsOf(summary),
					"rows":    output.RowsOf(result),
				})
			}
			if scored.Height() == 0 {
				fmt.Printf("%d 営業日後の足がまだありません\n", horizon)
				return nil
			}

			fmt.Printf("=== 倍率の帯ごとの実績（判断から %d 営業日後） ===\n", horizon)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "倍率の帯\t件数\t平均リターン bp\t勝率\t投下額")
			for _, row := range summary.Rows {
				fmt.Fprintf(w, "%v\t%d\t%+.1f\t%.1f%%\t%s\n",
					row["bucket"], row["count"], row["avg_ret_bp"],
					row["win_rate"].(float64)*100, yen(row["due"].(float64)))
			}
			w.Flush()
			if path != "" {
				fmt.Printf("履歴に追記 %s\n", path)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&horizon, "horizon", 20, "判断から何営業日後の終値で測るか")
	cmd.Flags().StringVar(&from, "from", "", "開始日 YYYY-MM-DD")
	cmd.Flags().StringVar(&to, "to", "", "終了日 YYYY-MM-DD")
	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}
