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

func newCheckCmd() *cobra.Command {
	var date string
	var days int
	var doNotify bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "営業日ごとの欠けを探す（欠けがあれば終了コード 2）",
		Long: "監視用。cron から回すときは --notify を付けると、ログを開かなくても気づける。\n" +
			"確認そのものに失敗したときも通知する（監視役が黙って死ぬのを防ぐ）。",
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

			s, err := newSession("check", false)
			if err != nil {
				return err
			}
			defer s.close()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "端点\t欠けている営業日")
			missingTotal := 0
			var lines []string
			for _, ep := range archive.StandardEndpoints {
				if ep.Mode != archive.ModeDate {
					continue
				}
				gaps, err := s.ingestor.Gaps(ep, start, end, clock.NowUTC())
				if err != nil {
					if doNotify {
						notify.Alert("jquants check が失敗", fmt.Sprintf("%s: %v", ep.Path, err), s.logger)
					}
					return fmt.Errorf("%s の確認に失敗しました: %w", ep.Path, err)
				}
				if len(gaps) == 0 {
					continue
				}
				missingTotal += len(gaps)
				shown := make([]string, 0, 8)
				for i, d := range gaps {
					if i >= 8 {
						break
					}
					shown = append(shown, d.Format("2006-01-02"))
				}
				text := strings.Join(shown, ", ")
				if len(gaps) > 8 {
					text += fmt.Sprintf(" …（計 %d）", len(gaps))
				}
				fmt.Fprintf(w, "%s\t%s\n", ep.Path, text)
				lines = append(lines, fmt.Sprintf("%s: %s", ep.Path, text))
			}
			if missingTotal == 0 {
				fmt.Printf("欠けはありません（%s 〜 %s）\n", start.Format("2006-01-02"), end.Format("2006-01-02"))
				return nil
			}
			fmt.Printf("欠け（%s 〜 %s）\n", start.Format("2006-01-02"), end.Format("2006-01-02"))
			w.Flush()
			if s.logger != nil {
				s.logger.Warn("jquants.gap", "欠けがあります", map[string]any{"missing": missingTotal})
			}
			if doNotify {
				notify.Alert(fmt.Sprintf("J-Quants の蓄積に欠け（%d 件）", missingTotal), strings.Join(lines, "\n"), s.logger)
			}
			s.close()
			os.Exit(2)
			return nil
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "確認する日（YYYY-MM-DD）。既定は今日")
	cmd.Flags().IntVar(&days, "days", 30, "欠けを探す範囲（日）")
	cmd.Flags().BoolVar(&doNotify, "notify", false, "欠けがあれば WBJP_ALERT_WEBHOOK_URL に通知する")
	return cmd
}
