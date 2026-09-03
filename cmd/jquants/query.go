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
	return &cobra.Command{
		Use:   "query [SQL]",
		Short: "DuckDB で端点 Parquet をビューにして SQL クエリを実行する",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sqlQuery := args[0]

			db, err := storage.OpenDuckDB()
			if err != nil {
				return fmt.Errorf("failed to open duckdb: %w", err)
			}
			defer db.Close()

			// 各端点ディレクトリの Parquet をビューとして登録
			arch, err := archive.OpenArchive(jquantsDir)
			if err == nil {
				defer arch.Close()
				dirs, _ := arch.ExistingParquetDirs()
				for _, dirName := range dirs {
					globPath := filepath.Join(jquantsDir, dirName, "*.parquet")
					createViewSQL := fmt.Sprintf("CREATE VIEW %s AS SELECT * FROM read_parquet('%s', union_by_name=true);", dirName, globPath)
					_, _ = db.Exec(createViewSQL)
				}
			}

			results, err := storage.QueryDuckDB(db, sqlQuery)
			if err != nil {
				return fmt.Errorf("query execution failed: %w", err)
			}

			if len(results) == 0 {
				fmt.Println("結果: 0 件")
				return nil
			}

			// カラム名一覧を取得
			var cols []string
			for k := range results[0] {
				cols = append(cols, k)
			}
			sort.Strings(cols)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			// ヘッダ
			fmt.Fprintln(w, strings.Join(cols, "\t"))
			// 各行
			for _, r := range results {
				var vals []string
				for _, c := range cols {
					vals = append(vals, fmt.Sprintf("%v", r[c]))
				}
				fmt.Fprintln(w, strings.Join(vals, "\t"))
			}
			w.Flush()
			return nil
		},
	}
}
