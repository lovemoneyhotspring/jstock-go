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

func newCheckCmd() *cobra.Command {
	var date string
	var days int
	var staleDays int
	var doNotify bool

	cmd := &cobra.Command{
		Use:   "check",
		Short: "営業日ごとの欠けと、古くなった端点を探す（あれば終了コード 2）",
		Long: "監視用。cron から回すときは --notify を付けると、ログを開かなくても気づける。\n" +
			"確認そのものに失敗したときも通知する（監視役が黙って死ぬのを防ぐ）。\n" +
			"日付で取る端点は営業日の欠けを、全件・範囲で取る端点（取引カレンダー・TOPIX・\n" +
			"決算予定・投資部門別）は最終取得が --stale-days より古いかを見る。\n" +
			"欠けを埋めるには `jquants repair`（同じ判定で、その日だけ取り直す）。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			start, end, err := archive.CheckRange(date, days)
			if err != nil {
				return err
			}

			s, err := newSession("check", false)
			if err != nil {
				return err
			}
			defer s.close()

			fail := func(what string, err error) error {
				if doNotify {
					notify.Alert("jquants check が失敗", fmt.Sprintf("%s: %v", what, err), s.logger)
				}
				return fmt.Errorf("%s の確認に失敗しました: %w", what, err)
			}
			now := clock.NowUTC()
			eps := archive.ActiveEndpoints()
			missingTotal := 0
			var lines, table []string
			for _, ep := range eps {
				if ep.Mode != archive.ModeDate {
					continue
				}
				gaps, err := s.ingestor.Gaps(ep, start, end, now)
				if err != nil {
					return fail(ep.Path, err)
				}
				if len(gaps) == 0 {
					continue
				}
				missingTotal += len(gaps)
				text := archive.JoinDays(gaps)
				table = append(table, fmt.Sprintf("%s\t%s", ep.Path, text))
				lines = append(lines, fmt.Sprintf("%s: %s", ep.Path, text))
			}
			stale, err := s.ingestor.Stale(eps, now, staleDays)
			if err != nil {
				return fail("最終取得", err)
			}
			staleTable, staleLines := staleRows(stale)
			table, lines = append(table, staleTable...), append(lines, staleLines...)

			span := fmt.Sprintf("%s 〜 %s", start.Format("2006-01-02"), end.Format("2006-01-02"))
			if missingTotal == 0 && len(stale) == 0 {
				fmt.Printf("欠けはありません（%s）\n", span)
				return nil
			}
			fmt.Printf("欠け・古い端点（%s）\n", span)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "端点\t欠けている営業日 / 最終取得")
			for _, row := range table {
				fmt.Fprintln(w, row)
			}
			w.Flush()
			if s.logger != nil {
				s.logger.Warn("jquants.gap", "欠けがあります", map[string]any{"missing": missingTotal, "stale": len(stale)})
			}
			if doNotify {
				notify.Alert(fmt.Sprintf("J-Quants の蓄積に欠け（%d 件、古い端点 %d）", missingTotal, len(stale)),
					strings.Join(lines, "\n"), s.logger)
			}
			return exitWith(2, "欠け %d 件、古い端点 %d（%s）", missingTotal, len(stale), strings.Join(lines, " / "))
		},
	}
	cmd.Flags().StringVar(&date, "date", "", "確認する日（YYYY-MM-DD、JST）。既定は JST の今日")
	cmd.Flags().IntVar(&days, "days", 30, "欠けを探す範囲（日）")
	cmd.Flags().IntVar(&staleDays, "stale-days", archive.DefaultStaleDays,
		"全件・範囲で取る端点を「古い」とみなす日数（取得間隔の 2 倍の方が長ければそちら）")
	cmd.Flags().BoolVar(&doNotify, "notify", false,
		"欠けがあれば Discord（"+notify.AlertChannelEnvVar+"）に通知する")
	return cmd
}
