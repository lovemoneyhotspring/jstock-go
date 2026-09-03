package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "jquants",
		Short: "J-Quants データの蓄積と横断クエリツール",
	}

	rootCmd.AddCommand(newStatusCmd())
	rootCmd.AddCommand(newCheckCmd())
	rootCmd.AddCommand(newSyncCmd())
	rootCmd.AddCommand(newBackfillCmd())
	rootCmd.AddCommand(newQueryCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
