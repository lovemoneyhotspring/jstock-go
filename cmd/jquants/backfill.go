package main

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/spf13/cobra"
)

func newBackfillCmd() *cobra.Command {
	var since string
	var only []string
	var noRaw bool

	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "一括ダウンロード（月次 csv.gz）で全期間を取り込む",
		Long: "初回に 1 回。再実行しても LastModified が変わったファイルだけ取り直す。\n" +
			"一括に無い端点（EDINET など）は `sync --days N` で遡る。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession("backfill", true)
			if err != nil {
				return err
			}
			defer s.close()

			targets, err := resolveEndpoints(only)
			if err != nil {
				return err
			}
			var ingests []archive.Ingest
			var failures []archive.Failure
			for _, ep := range targets {
				if !ep.Bulk {
					// --only で明示された端点だけ知らせる（既定は一括のあるものに絞る）
					if len(only) > 0 {
						fmt.Printf("%s は一括に無いので `sync --days N` で遡ります\n", ep.Path)
					}
					continue
				}
				fmt.Printf("%s を一括取り込み中…\n", ep.Path)
				result, err := s.ingestor.Backfill(ep, since, !noRaw)
				if err != nil {
					// 一覧の取得に失敗した等。他の端点は続ける
					failures = append(failures, archive.Failure{Endpoint: ep.Path, Target: "bulk:list", Error: err.Error()})
					continue
				}
				ingests = append(ingests, result.Ingests...)
				failures = append(failures, result.Failures...)
				rows := 0
				for _, r := range result.Ingests {
					rows += r.Rows
				}
				fmt.Printf("%s: %d ファイル、%d 行\n", ep.Path, len(result.Ingests), rows)
			}
			printIngests(ingests, "一括取り込み")
			if printFailures(failures, "再実行すればそこだけ取り直します") {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "この年月から（YYYY-MM）。省略時は取れる全期間")
	cmd.Flags().StringSliceVar(&only, "only", nil, "端点を絞る（名前かパス。複数可）")
	cmd.Flags().BoolVar(&noRaw, "no-raw", false, "一括 CSV を _raw/ に残さない")
	return cmd
}
