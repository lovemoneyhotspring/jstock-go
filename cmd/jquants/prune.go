package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/spf13/cobra"
)

func newPruneCmd() *cobra.Command {
	var only string
	var windowsSpec string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "日分割の端点（ティック）を時間帯で絞って容量を減らす",
		Long: "全時間帯で溜めて分析したあと、効く時間帯だけ残すためのもの。\n" +
			"時間帯は --windows か端点の環境変数（ティックは JQUANTS_TICKS_WINDOWS）。\n" +
			"落とした行は戻せない（2 年以内なら一括を取り直せる）。まず --dry-run で数える。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := newSession("prune", false)
			if err != nil {
				return err
			}
			defer s.close()

			ep, err := archive.LookupEndpoint(only)
			if err != nil {
				return err
			}
			var windows archive.Windows
			if windowsSpec != "" {
				windows, err = archive.ParseWindows(windowsSpec)
			} else {
				windows, err = ep.Windows()
			}
			if err != nil {
				return err
			}
			results, err := s.ingestor.Prune(ep, windows, dryRun)
			if err != nil {
				return err
			}
			title := fmt.Sprintf("刈り込み %s（残す時間帯: %s）", ep.Path, windows)
			if dryRun {
				title += " ※数えるだけ"
			}
			fmt.Println(title)
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ファイル\t前\t後\tMB\t書き換え")
			var before, after int
			var bytes int64
			for _, r := range results {
				mark := ""
				if r.Written {
					mark = "○"
				}
				fmt.Fprintf(w, "%s\t%d\t%d\t%.1f\t%s\n", r.Part, r.Before, r.After, float64(r.Bytes)/(1<<20), mark)
				before += r.Before
				after += r.After
				bytes += r.Bytes
			}
			fmt.Fprintf(w, "合計\t%d\t%d\t%.1f\t\n", before, after, float64(bytes)/(1<<20))
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&only, "only", "equities_trades", "端点（名前かパス）")
	cmd.Flags().StringVar(&windowsSpec, "windows", "", "残す時間帯（例: 09:00-09:10,15:10-15:31）。省略時は端点の環境変数")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "書き換えずに行数だけ数える")
	return cmd
}
