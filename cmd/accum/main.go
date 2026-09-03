package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "accum",
		Short: "日本株・指数の積立投資システム（売却・損切りはせず買い増しのみ）",
	}

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", appSettings.ConfigDir, "設定ディレクトリ")

	rootCmd.AddCommand(newStrategiesCmd())
	rootCmd.AddCommand(newListCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newOrdersCmd())
	rootCmd.AddCommand(newCompareCmd())
	rootCmd.AddCommand(newBackupCmd())
	rootCmd.AddCommand(newBacktestCmd())
	rootCmd.AddCommand(newRunCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
