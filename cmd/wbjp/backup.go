package main

import (
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/backup"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newBackupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "backup [destination]",
		Short: "売買データベース（SQLite）を別ファイルに複製する",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// ファイル名の時刻は JST（運用の記録・ログと同じ時計で探せるように）
			stamp := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("20060102-150405")
			dest := fmt.Sprintf("%s.backup-%s", appSettings.DBPath(), stamp)
			if len(args) > 0 {
				dest = args[0]
			}
			path, err := backupDB(appSettings.DBPath(), dest)
			if err != nil {
				return err
			}
			fmt.Printf("データベースをバックアップしました: %s\n", path)
			return nil
		},
	}
}

// backupDB は台帳を一貫したスナップショットとして複製し、所有者だけが読めるようにする。
//
// 台帳は WAL で書かれているので、ファイルを丸ごと読み書きすると本体に
// 反映前の書き込み（-wal 側）を落とし、書き込み途中も写しうる。VACUUM INTO で取る。
// 台帳には口座の注文・建玉が入るので 0600。既にあるファイルは上書きしない
// （BackupSQLite は複製先を消してから取り直すので、指定を誤ると別のファイルを失う）。
func backupDB(source, dest string) (string, error) {
	// 無い台帳を開くと空の DB ができてしまうので、先に確かめる
	if _, err := os.Stat(source); err != nil {
		return "", fmt.Errorf("データベースを読めません %s: %w", source, err)
	}
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("バックアップ先 %s は既にあります（上書きしません）", dest)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("バックアップ先を確かめられません %s: %w", dest, err)
	}
	path, err := backup.BackupSQLite(source, dest)
	if err != nil {
		return "", fmt.Errorf("バックアップ作成失敗: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("バックアップの権限を絞れません %s: %w", path, err)
	}
	return path, nil
}
