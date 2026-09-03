package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/spf13/cobra"
)

func newQueryCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "query [SQL]",
		Short: "DuckDB で端点 Parquet をビューにして SQL を実行する（研究用）",
		Long: "例: jquants query \"SELECT Code, Date, AdjC FROM equities_bars_daily " +
			"WHERE Code='72030' ORDER BY Date DESC LIMIT 5\"",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := storage.OpenDuckDB()
			if err != nil {
				return err
			}
			defer db.Close()

			// 端点ディレクトリの Parquet をビューとして登録する。
			// union_by_name で列が増えた月とも一緒に読める
			arch := archive.NewArchive(jquantsDir)
			for _, name := range arch.ExistingParquetDirs() {
				glob := filepath.Join(jquantsDir, name, "*.parquet")
				_, _ = db.Exec(fmt.Sprintf(
					"CREATE VIEW %s AS SELECT * FROM read_parquet('%s', union_by_name=true);", name, glob))
			}

			results, err := storage.QueryDuckDB(db, args[0])
			if err != nil {
				return fmt.Errorf("クエリの実行に失敗しました: %w", err)
			}
			if len(results) == 0 {
				fmt.Println("結果: 0 件")
				return nil
			}

			var cols []string
			for k := range results[0] {
				cols = append(cols, k)
			}
			sort.Strings(cols)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, strings.Join(cols, "\t"))
			shown := results
			if limit > 0 && len(shown) > limit {
				shown = shown[:limit]
			}
			for _, r := range shown {
				vals := make([]string, 0, len(cols))
				for _, c := range cols {
					if r[c] == nil {
						vals = append(vals, "")
						continue
					}
					vals = append(vals, fmt.Sprintf("%v", r[c]))
				}
				fmt.Fprintln(w, strings.Join(vals, "\t"))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(shown) < len(results) {
				fmt.Printf("…（全 %d 行のうち %d 行を表示）\n", len(results), len(shown))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "表示行数（0 で全件）")
	return cmd
}
