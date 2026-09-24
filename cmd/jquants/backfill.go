package main

import (
	"fmt"

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

			targets, err := archive.LookupEndpoints(only)
			if err != nil {
				return err
			}
			result := s.ingestor.BackfillAll(targets, since, !noRaw, func(step archive.BackfillStep) {
				switch {
				case step.Skipped:
					// --only で明示された端点だけ知らせる（既定は一括のあるものに絞る）
					if len(only) > 0 {
						fmt.Printf("%s は一括に無いので `sync --days N` で遡ります\n", step.Endpoint.Path)
					}
				case step.Err != nil:
					fmt.Printf("%s: 一括の一覧を取れません: %v\n", step.Endpoint.Path, step.Err)
				default:
					rows := 0
					for _, r := range step.Result.Ingests {
						rows += r.Rows
					}
					fmt.Printf("%s: %d ファイル、%d 行\n", step.Endpoint.Path, len(step.Result.Ingests), rows)
				}
			})
			printIngests(result.Ingests, "一括取り込み")
			noteIngests(result.Ingests, result.Failures)
			if printFailures(result.Failures, "再実行すればそこだけ取り直します") {
				return exitWith(1, "%s", failureSummary(result.Failures))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "この年月から（YYYY-MM）。省略時は取れる全期間")
	cmd.Flags().StringSliceVar(&only, "only", nil, "端点を絞る（名前かパス。複数可）")
	cmd.Flags().BoolVar(&noRaw, "no-raw", false, "一括 CSV を _raw/ に残さない")
	return cmd
}
