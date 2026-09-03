package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newBackfillCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backfill",
		Short: "一括ダウンロードで全期間のデータを取り込む",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiKey := os.Getenv("WBJP_JQUANTS_API_KEY")
			if apiKey == "" {
				if v, ok := appSettings.DotenvMap["WBJP_JQUANTS_API_KEY"]; ok {
					apiKey = v
				}
			}
			if apiKey == "" {
				return fmt.Errorf("J-Quants APIキーが設定されていません（WBJP_JQUANTS_API_KEY）")
			}
			fmt.Println("全期間の一括バルク取り込みは J-Quants サイトの月次アーカイブからダウンロードします。")
			return nil
		},
	}
}
