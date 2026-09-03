package main

import (
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "accum",
		Short: "日本株・指数の積立投資システム（売却・損切りはせず買い増しのみ）",
		// 入口で 1 回だけ run_id を発行し、ログ・履歴・ダイジェストで共有する。
		// コマンドごとに発行すると、同じ実行の記録が突き合わせられなくなる。
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			run = cli.StartRun("accum", appSettings, cmd.Name())
			runCtx = run.Ctx
		},
	}

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", appSettings.ConfigDir, "設定ディレクトリ")
	rootCmd.PersistentFlags().StringVar(&logLevelFlag, "log-level", "info", "ログレベル（debug / info / warn / error）")
	rootCmd.PersistentFlags().BoolVar(&jsonLogsFlag, "json-logs", false, "端末にも JSON で出力")

	rootCmd.AddCommand(newStrategiesCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newOrdersCmd())
	rootCmd.AddCommand(newCompareCmd())
	rootCmd.AddCommand(newBackupCmd())
	rootCmd.AddCommand(newBacktestCmd())
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newHistoryCmd())
	rootCmd.AddCommand(newEvaluateCmd())
	rootCmd.AddCommand(newBasketCmd())
	rootCmd.AddCommand(cli.NewPendingCmd("accum", appSettings.AccumDBPath))

	// os.Exit は defer を飛ばすので、ダイジェストはここで必ず書き出す。
	// 「今日ちゃんと動いたか」を AI が 1 ファイルで読めることが目的なので、
	// 失敗した実行こそ残さなければならない。
	err := rootCmd.Execute()
	run.Finish(err)
	if err != nil {
		os.Exit(1)
	}
}
