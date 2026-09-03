package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
	"github.com/spf13/cobra"
)

func main() {
	appSettings := settings.LoadAppSettings()
	jquantsDir := filepath.Join(appSettings.DataDir, "jquants")

	var rootCmd = &cobra.Command{
		Use:   "jquants",
		Short: "J-Quants データの蓄積と横断クエリツール",
	}

	// status サブコマンド
	var cmdStatus = &cobra.Command{
		Use:   "status",
		Short: "端点ごとの蓄積状況を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			arch, err := archive.OpenArchive(jquantsDir)
			if err != nil {
				return err
			}
			defer arch.Close()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "端点\t最古\t最新\t件数")
			for _, ep := range archive.StandardEndpoints {
				oldest, latest, count, err := arch.EndpointStatus(ep.Name)
				if err != nil {
					oldest, latest = "-", "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", ep.Name, oldest, latest, count)
			}
			w.Flush()
			return nil
		},
	}

	// check サブコマンド
	var cmdCheck = &cobra.Command{
		Use:   "check",
		Short: "直近営業日のデータが揃っているか確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			arch, err := archive.OpenArchive(jquantsDir)
			if err != nil {
				return err
			}
			defer arch.Close()

			fmt.Println("直近データの整合性を確認中...")
			allGood := true
			for _, ep := range archive.StandardEndpoints {
				_, latest, count, _ := arch.EndpointStatus(ep.Name)
				if count == 0 {
					fmt.Printf("[未取得] %s\n", ep.Name)
					allGood = false
				} else {
					fmt.Printf("[正常] %s: 最新 %s (%d件)\n", ep.Name, latest, count)
				}
			}
			if !allGood {
				fmt.Println("\n未取得の端点があります。'jquants sync' または 'jquants backfill' を実行してください。")
			}
			return nil
		},
	}

	// query サブコマンド (DuckDB 利用)
	var cmdQuery = &cobra.Command{
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

	rootCmd.AddCommand(cmdStatus)
	rootCmd.AddCommand(cmdCheck)
	rootCmd.AddCommand(cmdQuery)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
