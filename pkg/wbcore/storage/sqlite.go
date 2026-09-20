package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// OpenSQLite は指定パスの SQLite データベースを開き、WAL モードと外部キー制約を有効にする。
func OpenSQLite(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory for db %s: %w", dbPath, err)
	}

	// foreign_keys と busy_timeout は**接続単位**の設定。db.Exec で 1 回流すだけだと、プールが
	// 2 本目の接続を開いたときに効いていない（busy_timeout 0 = 他のプロセスが書いている間は
	// 即 SQLITE_BUSY）。DSN の _pragma に書けば、ドライバが接続を開くたびに流す。
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db at %s: %w", dbPath, err)
	}

	// WAL はファイルに残る設定なので 1 回でよい
	if _, err := db.Exec("PRAGMA journal_mode = WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to execute pragma journal_mode: %w", err)
	}

	return db, nil
}
