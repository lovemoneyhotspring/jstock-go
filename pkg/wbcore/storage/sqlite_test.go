package storage

import (
	"context"
	"path/filepath"
	"testing"
)

// busy_timeout と foreign_keys は接続単位。プールが開く 2 本目の接続にも効いていること。
func TestOpenSQLitePragmasApplyToEveryConnection(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "日本語 のパス", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	// 1 本目を握ったまま 2 本目を取る（プールに新しい接続を開かせる）
	for i := 0; i < 2; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var timeout, fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if timeout != 5000 || fk != 1 {
			t.Errorf("接続 %d: busy_timeout=%d foreign_keys=%d, want 5000 / 1", i+1, timeout, fk)
		}
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q err=%v, want wal", mode, err)
	}
}
