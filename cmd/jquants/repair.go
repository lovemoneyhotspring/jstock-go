package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	"github.com/spf13/cobra"
)

func newRepairCmd() *cobra.Command {
	var date string
	var days int
	var only []string
	var dryRun bool
	var doNotify bool

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "check と同じ判定で欠けを探し、その日だけ取り直す（埋まらなければ終了コード 2）",
		Long: "check が欠けを報告したときの修復用。API のある端点は 1 日ずつ date= で取り直す\n" +
			"（0 行でも台帳に残るので、週次のように行の無い日は次から欠けと数えない）。\n" +
			"API の無い端点（ティック）は一括の日次ファイルを欠けの月から取り直す。\n" +
			"--dry-run で取らずに対象だけ表示する。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			end := clock.TodayUTC()
			if date != "" {
				parsed, err := time.Parse("2006-01-02", date)
				if err != nil {
					return fmt.Errorf("--date は YYYY-MM-DD で指定してください: %w", err)
				}
				end = parsed
			}
			start := end.AddDate(0, 0, -days)
			eps, err := resolveEndpoints(only)
			if err != nil {
				return err
			}

			s, err := newSession("repair", !dryRun)
			if err != nil {
				return err
			}
			defer s.close()
			now := clock.NowUTC()
			span := fmt.Sprintf("%s 〜 %s", start.Format("2006-01-02"), end.Format("2006-01-02"))

			if dryRun {
				plans, err := s.ingestor.PlanRepair(eps, start, end, now)
				if err != nil {
					return err
				}
				if len(plans) == 0 {
					fmt.Printf("欠けはありません（%s）\n", span)
					return nil
				}
				fmt.Printf("取り直す日（%s）\n", span)
				printPlans(plans)
				return nil
			}

			result, err := s.ingestor.Repair(eps, start, end, now)
			if err != nil {
				if doNotify {
					notify.Alert("jquants repair が失敗", err.Error(), s.logger)
				}
				return err
			}
			if len(result.Plans) == 0 {
				fmt.Printf("欠けはありません（%s）\n", span)
				return nil
			}
			printIngests(result.Ingests, fmt.Sprintf("取り直し（%s）", span))
			failed := printFailures(result.Failures, "もう一度 repair を実行してください")
			if len(result.Remaining) == 0 {
				fmt.Println("欠けはすべて埋まりました")
				if failed {
					os.Exit(1)
				}
				return nil
			}
			fmt.Println("まだ欠けている日")
			printPlans(result.Remaining)
			total := 0
			var lines []string
			for _, p := range result.Remaining {
				total += len(p.Days)
				lines = append(lines, fmt.Sprintf("%s: %s", p.Endpoint.Path, joinDays(p.Days)))
			}
			if s.logger != nil {
				s.logger.Warn("jquants.gap", "取り直しても欠けが残っています", map[string]any{"missing": total})
			}
			if doNotify {
				notify.Alert(fmt.Sprintf("J-Quants の欠けが埋まらない（%d 件）", total), strings.Join(lines, "\n"), s.logger)
			}
			s.close()
			os.Exit(2)
			return nil
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "確認する日（YYYY-MM-DD）。既定は今日")
	cmd.Flags().IntVar(&days, "days", 30, "欠けを探す範囲（日）")
	cmd.Flags().StringSliceVar(&only, "only", nil, "端点を絞る（名前かパス。複数可）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "取らずに、対象の日だけ表示")
	cmd.Flags().BoolVar(&doNotify, "notify", false,
		"埋まらなかったとき Discord（"+notify.AlertChannelEnvVar+"）に通知する")
	return cmd
}

// printPlans は端点ごとの欠けを表で出す（先頭 8 日と合計）。
func printPlans(plans []archive.RepairPlan) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "端点\t欠けている営業日")
	for _, p := range plans {
		fmt.Fprintf(w, "%s\t%s\n", p.Endpoint.Path, joinDays(p.Days))
	}
	w.Flush()
}

// joinDays は日付を先頭 8 つまで並べ、多ければ合計を添える。
func joinDays(days []time.Time) string {
	shown := make([]string, 0, 8)
	for i, d := range days {
		if i >= 8 {
			break
		}
		shown = append(shown, d.Format("2006-01-02"))
	}
	text := strings.Join(shown, ", ")
	if len(days) > 8 {
		text += fmt.Sprintf(" …（計 %d）", len(days))
	}
	return text
}
