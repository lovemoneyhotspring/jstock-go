package main

import (
	"fmt"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"os"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	corehistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/output"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/evaluate"
	wbjphistory "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/history"
	"github.com/spf13/cobra"
)

func newEvaluateCmd() *cobra.Command {
	var (
		date          string
		horizon, days int
		asJSON        bool
	)

	cmd := &cobra.Command{
		Use:   "evaluate",
		Short: "スクリーニングの結果に後日の足を当て、選定が効いたかを履歴に残す",
		Long: "スクリーニングの結果に後日の足を当て、選定が効いたかを履歴に残す。\n\n" +
			"採用した銘柄（adopted）だけを見ても相場全体の上下と区別できないので、\n" +
			"同じ日に候補へ挙がったが採用しなかった銘柄と並べて比べる。材料は\n" +
			"wbjp screen が積んだ履歴で、判断のロジックには触れない。",
		RunE: func(cmd *cobra.Command, args []string) error {
			store := wbjphistory.StoreFor(appSettings)
			bars := data.NewBarStore(appSettings.BarsDir())

			known := store.Days(wbjphistory.Kind)
			if len(known) == 0 {
				message := "スクリーニングの履歴がありません（wbjp screen を回す）"
				if asJSON {
					return output.EmitJSON(map[string]any{"ok": true, "days": []any{}, "message": message})
				}
				fmt.Println(message)
				return nil
			}

			// 評価する判断日。--date が無ければ、まだ評価していない日を古い順に全部。
			// 以前は「直近 5 判断日」だけを見ていたので、horizon（20 営業日）後の足が揃った
			// 日は窓から外れて評価されず、窓の中の日は晩ごとに同じ日を積み直していた
			// （2026-09-24 のレビュー）。評価済みかは同じ horizon で実績の出た行があるかで見る
			targets := known
			if date != "" {
				day, err := cli.ParseDay(date)
				if err != nil {
					return err
				}
				targets = []time.Time{day}
			} else {
				past, err := store.Read(evaluate.Kind, corehistory.Range{})
				if err != nil {
					return err
				}
				done := evaluate.EvaluatedDays(past, horizon)
				targets = nil
				for _, day := range known {
					if !done[day] {
						targets = append(targets, day)
					}
				}
				if days > 0 && len(targets) > days {
					targets = targets[len(targets)-days:]
				}
			}

			written := []map[string]any{}
			for _, day := range targets {
				screens, err := store.Latest(wbjphistory.Kind, day)
				if err != nil {
					return err
				}
				if screens.Height() == 0 {
					continue
				}

				result := evaluate.Evaluate(screens, bars, day, horizon)
				if evaluate.Scored(result).Height() == 0 {
					// horizon 本先の足がまだ無い日。次に回せば評価できるので残さない
					if !asJSON {
						fmt.Printf("%s: %d 営業日後の足がまだありません\n", day.Format("2006-01-02"), horizon)
					}
					continue
				}

				path, err := store.Append(evaluate.Kind, result, day, corehistory.AppendOptions{})
				if err != nil {
					return err
				}
				written = append(written, map[string]any{
					"day": day.Format("2006-01-02"), "rows": result.Height(), "path": path,
				})
				if !asJSON {
					fmt.Printf("\n%s  %d 営業日後  %s\n", day.Format("2006-01-02"), horizon, path)
					printGroupSummary(evaluate.Summarize(result))
				}
			}

			if asJSON {
				return output.EmitJSON(map[string]any{"ok": true, "horizon": horizon, "days": written})
			}
			if len(written) == 0 {
				fmt.Println("評価できる判断日がありませんでした")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&date, "date", "", "評価する判断日 YYYY-MM-DD")
	cmd.Flags().IntVar(&horizon, "horizon", 20, "判断から何営業日後の終値で測るか")
	cmd.Flags().IntVar(&days, "days", 0, "--date を省いたとき、未評価の判断日のうち新しい何日に絞るか（0 なら全部）")
	cmd.Flags().BoolVar(&asJSON, "json", false, "表の代わりに JSON を 1 個だけ出す（AI・パイプ用）")
	return cmd
}

// printGroupSummary は群ごとの実績を出す。adopted が rest を上回っていれば選定が効いている。
func printGroupSummary(summary corehistory.Frame) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "群\t件数\t平均リターン bp\t勝率")
	for _, row := range summary.Rows {
		fmt.Fprintf(w, "%v\t%v\t%s\t%s\n",
			row["group"], row["count"], signedBP(row["avg_ret_bp"]), percent(row["win_rate"]))
	}
	w.Flush()
}

// signedBP は符号付きの bp 表示。欠損は "-"。
func signedBP(value any) string {
	f, ok := value.(float64)
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%+.1f", f)
}

func percent(value any) string {
	f, ok := value.(float64)
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", f*100)
}
