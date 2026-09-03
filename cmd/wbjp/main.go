package main

import (
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "wbjp",
		Short: "日本株スイング売買システム（立花証券 e支店 API / Paper）",
		// 入口で 1 回だけ run_id を発行し、ログ・DB・ダイジェストで共有する
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			run = cli.StartRun("wbjp", appSettings, cmd.Name())
		},
	}

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", appSettings.ConfigDir, "設定ディレクトリ")

	rootCmd.AddCommand(newAccountCmd())
	rootCmd.AddCommand(newStopsCmd())
	rootCmd.AddCommand(newScreenCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newStrategiesCmd())
	rootCmd.AddCommand(newOrdersCmd())
	rootCmd.AddCommand(newCancelCmd())
	rootCmd.AddCommand(newBackupCmd())
	rootCmd.AddCommand(newBacktestCmd())
	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newOrderCmd())
	rootCmd.AddCommand(newHistoryCmd())
	rootCmd.AddCommand(newEvaluateCmd())
	rootCmd.AddCommand(newReviewCmd())
	rootCmd.AddCommand(newExplainCmd())
	rootCmd.AddCommand(newRunsCmd())
	rootCmd.AddCommand(newDataCmd())
	rootCmd.AddCommand(newCredentialsCmd())
	rootCmd.AddCommand(cli.NewPendingCmd("wbjp", appSettings.DBPath))

	// os.Exit は defer を飛ばすので、ダイジェストはここで必ず書き出す
	err := rootCmd.Execute()
	run.Finish(err)
	if err != nil {
		os.Exit(1)
	}
}
