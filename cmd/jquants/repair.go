package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	"github.com/spf13/cobra"
)

func newRepairCmd() *cobra.Command {
	var date string
	var days int
	var staleDays int
	var only []string
	var dryRun bool
	var doNotify bool

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "check と同じ判定で欠けを探し、その日だけ取り直す（埋まらない・古い端点があれば終了コード 3）",
		Long: "check が欠けを報告したときの修復用。API のある端点は 1 日ずつ date= で取り直す\n" +
			"（0 行でも台帳に残るので、週次のように行の無い日は次から欠けと数えない。\n" +
			"日足のように毎営業日行があるはずの端点は、0 行なら欠けのまま残る）。\n" +
			"API の無い端点（ティック）は一括の日次ファイルを欠けの月から取り直す。\n" +
			"終わりに check と同じく、全件・範囲で取る端点（取引カレンダー・TOPIX・決算予定・\n" +
			"投資部門別）の最終取得が --stale-days より古くないかも見る（cron の 20:00 の監視はこれ 1 本）。\n" +
			"--dry-run で取らずに対象だけ表示する。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			start, end, err := archive.CheckRange(date, days)
			if err != nil {
				return err
			}
			eps, err := archive.LookupEndpoints(only)
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
			fail := func(err error) error {
				if doNotify {
					notify.Alert("jquants repair が失敗", err.Error(), s.logger)
				}
				return err
			}

			if dryRun {
				plans, err := s.ingestor.PlanRepair(eps, start, end, now)
				if err != nil {
					return err
				}
				stale, err := s.ingestor.Stale(eps, now, staleDays)
				if err != nil {
					return err
				}
				if len(plans) == 0 {
					fmt.Printf("欠けはありません（%s）\n", span)
				} else {
					fmt.Printf("取り直す日（%s）\n", span)
					printPlans(plans)
				}
				printStale(stale)
				return nil
			}

			result, err := s.ingestor.Repair(eps, start, end, now)
			if err != nil {
				return fail(err)
			}
			// 全件・範囲で取る端点は日の欠けを持たないので、最終取得の古さで見る。
			// 以前は 20:00 の cron が check --notify で見ていたが、repair --notify に替えたとき（0134076）に抜けた
			stale, err := s.ingestor.Stale(eps, now, staleDays)
			if err != nil {
				return fail(fmt.Errorf("最終取得の確認に失敗しました: %w", err))
			}
			noteIngests(result.Ingests, result.Failures)

			if len(result.Plans) == 0 {
				fmt.Printf("欠けはありません（%s）\n", span)
			} else {
				printIngests(result.Ingests, fmt.Sprintf("取り直し（%s）", span))
			}
			failed := printFailures(result.Failures, "もう一度 repair を実行してください")
			if len(result.Plans) > 0 && len(result.Remaining) == 0 {
				fmt.Println("欠けはすべて埋まりました")
			}
			total := 0
			var lines []string
			if len(result.Remaining) > 0 {
				fmt.Println("まだ欠けている日")
				printPlans(result.Remaining)
				for _, p := range result.Remaining {
					total += len(p.Days)
					lines = append(lines, fmt.Sprintf("%s: %s", p.Endpoint.Path, archive.JoinDays(p.Days)))
				}
				if s.logger != nil {
					s.logger.Warn("jquants.gap", "取り直しても欠けが残っています", map[string]any{"missing": total})
				}
			}
			printStale(stale)
			if len(stale) > 0 {
				_, staleLines := staleRows(stale)
				lines = append(lines, staleLines...)
				if s.logger != nil {
					s.logger.Warn("jquants.stale", "最終取得の古い端点があります", map[string]any{"stale": len(stale)})
				}
			}
			if total > 0 || len(stale) > 0 {
				title := fmt.Sprintf("J-Quants の欠けが埋まらない（%d 件）", total)
				if len(stale) > 0 {
					title = fmt.Sprintf("J-Quants の欠けが埋まらない（%d 件、古い端点 %d）", total, len(stale))
				}
				if doNotify {
					notify.Alert(title, strings.Join(lines, "\n"), s.logger)
				}
				return exitWith(exitGaps, "欠けが埋まらない %d 件、古い端点 %d（%s）", total, len(stale), strings.Join(lines, " / "))
			}
			if failed {
				return exitWith(1, "%s", failureSummary(result.Failures))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "確認する日（YYYY-MM-DD、JST）。既定は JST の今日")
	cmd.Flags().IntVar(&days, "days", 30, "欠けを探す範囲（日）")
	cmd.Flags().IntVar(&staleDays, "stale-days", archive.DefaultStaleDays,
		"全件・範囲で取る端点を「古い」とみなす日数（取得間隔の 2 倍の方が長ければそちら）")
	cmd.Flags().StringSliceVar(&only, "only", nil, "端点を絞る（名前かパス。複数可）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "取らずに、対象の日だけ表示")
	cmd.Flags().BoolVar(&doNotify, "notify", false,
		"埋まらない・古い端点があるとき Discord（"+notify.AlertChannelEnvVar+"）に通知する")
	return cmd
}

// printStale は最終取得の古い端点を表で出す。無ければ何も出さない。
func printStale(stale []archive.Stale) {
	if len(stale) == 0 {
		return
	}
	table, _ := staleRows(stale)
	fmt.Println("最終取得の古い端点")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "端点\t最終取得")
	for _, row := range table {
		fmt.Fprintln(w, row)
	}
	w.Flush()
}

// printPlans は端点ごとの欠けを表で出す（先頭 8 日と合計）。
func printPlans(plans []archive.RepairPlan) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "端点\t欠けている営業日")
	for _, p := range plans {
		fmt.Fprintf(w, "%s\t%s\n", p.Endpoint.Path, archive.JoinDays(p.Days))
	}
	w.Flush()
}
