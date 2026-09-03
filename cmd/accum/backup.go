package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/backup"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/notify"
	"github.com/spf13/cobra"
)

func newBackupCmd() *cobra.Command {
	var dest string
	var keep int

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "state/ の全 SQLite（積立台帳・スイング売買の記録）を日付付きで複製する",
		Long: "台帳は「今月いくら発注済みか」の唯一の記録で、ブローカーから再構築できない。\n" +
			"失うと次の実行で当月の予算を買い直す。wbjp の注文・シグナル履歴も同様に\n" +
			"このホストにしか無いので、まとめて複製する。cron で毎日回す。",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, err := newRunLogger("backup")
			if err != nil {
				return err
			}
			defer logger.Close()

			result, err := backup.BackupState(appSettings, backup.Options{Dest: dest, Keep: keep})
			if err != nil {
				// cron の中で黙って落ちると、バックアップの無い日が続いても気づけない。
				// 通知して次の実行の再試行に任せる
				notify.Alert("state のバックアップに失敗", err.Error(), logger)
				digest.Anomaly("accum.backup_failed", err.Error())
				return fmt.Errorf("バックアップに失敗しました: %w", err)
			}

			for _, path := range result.Copied {
				fmt.Printf("複製しました: %s\n", path)
			}
			if len(result.Copied) == 0 {
				fmt.Printf("複製対象がありません（%s/*.db が空）\n", appSettings.StateDir)
			}
			if len(result.Removed) > 0 {
				fmt.Printf("古い複製を %d 件削除\n", len(result.Removed))
			}
			digest.Note(map[string]any{"copied": len(result.Copied), "removed": len(result.Removed)})
			return nil
		},
	}

	cmd.Flags().StringVar(&dest, "dest", "", "保存先ディレクトリ（既定 state/backup）")
	cmd.Flags().IntVar(&keep, "keep", backup.DefaultKeep, "元ファイルごとに残す世代数。古いものから消す")
	return cmd
}
