package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
)

// TestBackupDBSnapshotsOwnerOnly は、書き込み中（WAL が残っている）の台帳を一貫した
// 複製として取り、所有者だけが読める権限にし、既にあるファイルは上書きしないこと。
func TestBackupDBSnapshotsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "wbjp.db")
	rep, err := repo.OpenRepo(src)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Close() // 開いたまま（WAL に書きかけ）で複製する
	if err := rep.StartRun("run-1", "2026-09-14", "prod", "live"); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backup", "copy.db")
	path, err := backupDB(src, dest)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("権限 %o（期待 600）", perm)
	}
	copied, err := repo.OpenRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if got, err := copied.GetRun("run-1"); err != nil || got == nil {
		t.Errorf("WAL 側の書き込みが複製に無い: %v %v", got, err)
	}

	if _, err := backupDB(src, dest); err == nil {
		t.Error("既にあるバックアップを上書きした")
	}

	missing := filepath.Join(dir, "none.db")
	if _, err := backupDB(missing, filepath.Join(dir, "x.db")); err == nil {
		t.Error("無い台帳のバックアップが通った")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("無い台帳を空の DB として作ってしまった")
	}
}
