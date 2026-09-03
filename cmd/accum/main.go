package main

import (
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "accum",
		Short: "日本株・指数の積立投資システム（売却・損切りはせず買い増しのみ）",
		// 入口で 1 回だけ run_id を発行し、ログ・履歴・ダイジェストで共有する。
		// コマンドごとに発行すると、同じ実行の記録が突き合わせられなくなる。
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			ctx, runID := logging.BindRunContext(cmd.Context(), map[string]any{
				"app": "accum", "env": string(appSettings.Env), "command": cmd.Name(),
			})
			runCtx = ctx
			digest.StartRun(digest.StartOptions{
				App:      "accum",
				Env:      string(appSettings.Env),
				Command:  cmd.Name(),
				RunID:    runID,
				StateDir: appSettings.StateDir,
			})
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

	err := rootCmd.Execute()
	// os.Exit は defer を飛ばすので、ダイジェストはここで必ず書き出す。
	// 「今日ちゃんと動いたか」を AI が 1 ファイルで読めることが目的なので、
	// 失敗した実行こそ残さなければならない。
	if err != nil {
		digest.Fail("accum.command_failed", err.Error())
	}
	_ = digest.Flush()
	if err != nil {
		os.Exit(1)
	}
}
