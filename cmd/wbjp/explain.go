package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/spf13/cobra"
)

func newExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain [run_id]",
		Short: "ある実行の判断過程を丸ごと表示する",
		Long: "ある実行の判断過程を丸ごと表示する。\n\n" +
			"シグナル → 合成 → 目標 → 注文 → 拒否理由 の順に、\n" +
			"「なぜそうなったか」を追える。",
		Args: cobra.ExactArgs(1),
		// 実行や注文が見つからないのは使い方の誤りではないので、使い方は出さない
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := args[0]
			r, err := repo.OpenRepo(appSettings.DBPath())
			if err != nil {
				return err
			}
			defer r.Close()

			run, err := r.GetRun(runID)
			if err != nil {
				return err
			}
			if run == nil {
				return fmt.Errorf("実行 %s が見つかりません", runID)
			}

			sections, err := r.Explain(runID)
			if err != nil {
				return err
			}
			loc := clock.MustZone(appSettings.Timezone)

			for _, section := range sections {
				if len(section.Rows) == 0 {
					continue
				}
				fmt.Printf("\n[%s]\n", section.Name)
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, strings.Join(section.Columns, "\t"))
				for _, row := range section.Rows {
					cells := make([]string, len(section.Columns))
					for i, column := range section.Columns {
						cells[i] = explainCell(column, row[column], loc)
					}
					fmt.Fprintln(w, strings.Join(cells, "\t"))
				}
				w.Flush()
			}
			return nil
		},
	}
}

// explainCell は 1 セルの表示。
//
// 時刻の列（*_at）は保存が UTC の ISO なので、表示は設定の時間帯に直す。
// 長い列（meta_json など）は 40 文字で切る——1 行に収まらないと流れが読めない。
func explainCell(column string, value any, loc *time.Location) string {
	if value == nil {
		return ""
	}
	text := fmt.Sprint(value)
	if strings.HasSuffix(column, "_at") {
		text = clock.FmtISO(text, loc)
	}
	if len(text) > 40 {
		text = text[:40]
	}
	return text
}
