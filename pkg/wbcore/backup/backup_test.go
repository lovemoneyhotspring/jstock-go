package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

func makeLedger(t *testing.T, path string, rows int) {
	t.Helper()
	db, err := storage.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE orders (id INTEGER PRIMARY KEY, symbol TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := db.Exec("INSERT INTO orders (symbol) VALUES (?)", "7203"); err != nil {
			t.Fatal(err)
		}
	}
}

func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM orders").Scan(&n); err != nil {
		t.Fatalf("バックアップが読めない %s: %v", path, err)
	}
	return n
}

func newSettings(t *testing.T) *settings.AppSettings {
	t.Helper()
	dir := t.TempDir()
	return &settings.AppSettings{Env: settings.EnvUAT, StateDir: filepath.Join(dir, "state"), DataDir: filepath.Join(dir, "data")}
}

func TestBackupStateCopiesAllLedgers(t *testing.T) {
	s := newSettings(t)
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	makeLedger(t, filepath.Join(s.StateDir, "accum-uat.db"), 3)
	makeLedger(t, filepath.Join(s.StateDir, "wbjp-uat.db"), 1)
	// ログは対象外
	if err := os.WriteFile(filepath.Join(s.StateDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	today := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	result, err := BackupState(s, Options{Keep: DefaultKeep, Today: today})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Copied) != 2 {
		t.Fatalf("複製 = %v", result.Copied)
	}
	accum := filepath.Join(s.BackupDir(), "accum-uat-20260903.db")
	if result.Copied[0] != accum {
		t.Fatalf("複製先の名前 = %s", result.Copied[0])
	}
	// 中身が読めること（＝一貫したスナップショットになっていること）
	if n := countRows(t, accum); n != 3 {
		t.Errorf("行数 = %d, want 3", n)
	}
}

func TestBackupStateIsIdempotentWithinADay(t *testing.T) {
	s := newSettings(t)
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	makeLedger(t, filepath.Join(s.StateDir, "accum-uat.db"), 1)
	today := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)

	// cron の再試行や手動実行で同じ日に 2 回走っても失敗しない（取り直す）
	for i := 0; i < 2; i++ {
		if _, err := BackupState(s, Options{Keep: 30, Today: today}); err != nil {
			t.Fatalf("%d 回目: %v", i+1, err)
		}
	}
	entries, err := os.ReadDir(s.BackupDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("1 日 1 世代のはずが %d 個", len(entries))
	}
}

func TestBackupStatePrunesOldGenerations(t *testing.T) {
	s := newSettings(t)
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	makeLedger(t, filepath.Join(s.StateDir, "accum-uat.db"), 1)

	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var last Result
	for i := 0; i < 5; i++ {
		result, err := BackupState(s, Options{Keep: 3, Today: base.AddDate(0, 0, i)})
		if err != nil {
			t.Fatal(err)
		}
		last = result
	}
	entries, err := os.ReadDir(s.BackupDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("残った世代 = %d, want 3", len(entries))
	}
	if entries[0].Name() != "accum-uat-20260903.db" {
		t.Errorf("古い世代から消えていない: %s", entries[0].Name())
	}
	if len(last.Removed) != 1 {
		t.Errorf("削除の記録 = %v", last.Removed)
	}
}

func TestKeepZeroKeepsEverything(t *testing.T) {
	s := newSettings(t)
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	makeLedger(t, filepath.Join(s.StateDir, "accum-uat.db"), 1)
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := BackupState(s, Options{Keep: 0, Today: base.AddDate(0, 0, i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(s.BackupDir())
	if len(entries) != 3 {
		t.Fatalf("keep=0 なら削らない: %d", len(entries))
	}
}

func TestMissingStateDirIsNotAnError(t *testing.T) {
	s := newSettings(t)
	result, err := BackupState(s, Options{Keep: 30})
	if err != nil {
		t.Fatalf("state が無いだけで失敗すべきではない: %v", err)
	}
	if len(result.Copied) != 0 {
		t.Errorf("複製 = %v", result.Copied)
	}
}
