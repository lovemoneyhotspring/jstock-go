package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "wbjp",
		Short: "日本株スイング売買システム（立花証券 e支店 API / Paper）",
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

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
