package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

func newBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup [destination]",
		Short: "積立台帳データベース（SQLite）を別ファイルに複製する",
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := fmt.Sprintf("%s.backup-%s", appSettings.AccumDBPath(), time.Now().Format("20060102-150405"))
			if len(args) > 0 {
				dest = args[0]
			}
			srcData, err := os.ReadFile(appSettings.AccumDBPath())
			if err != nil {
				return fmt.Errorf("DB読み込み失敗: %w", err)
			}
			if err := os.WriteFile(dest, srcData, 0644); err != nil {
				return fmt.Errorf("バックアップ作成失敗: %w", err)
			}
			fmt.Printf("積立台帳をバックアップしました: %s\n", dest)
			return nil
		},
	}
}
