package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrateAppliesOnceAndTracksVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	applied := 0
	migrations := []Migration{
		{Name: "t", Up: Exec("CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY)")},
		{Name: "t.name", Up: func(tx *sql.Tx) error {
			applied++
			return AddColumn(tx, "t", "name", "TEXT NOT NULL DEFAULT ''")
		}},
	}
	for i := 0; i < 2; i++ {
		db, err := OpenSQLite(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := Migrate(db, migrations); err != nil {
			t.Fatalf("%d 回目: %v", i+1, err)
		}
		v, _ := UserVersion(db)
		if v != 2 {
			t.Errorf("版 = %d, want 2", v)
		}
		db.Close()
	}
	if applied != 1 {
		t.Errorf("2 段目が %d 回走った（1 回のはず）", applied)
	}
}

func TestMigrateUpgradesLegacyDB(t *testing.T) {
	// 版 0 で表だけある（ALTER で列を足す前の台帳）
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	migrations := []Migration{
		{Name: "t", Up: Exec("CREATE TABLE IF NOT EXISTS t (id INTEGER PRIMARY KEY, name TEXT)")},
		{Name: "t.name", Up: AddColumns("t", map[string]string{"name": "TEXT"})},
	}
	if err := Migrate(db, migrations); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t (id, name) VALUES (1, 'x')"); err != nil {
		t.Fatalf("name 列が足されていない: %v", err)
	}
	// 新しい DB で 2 段目を走らせても（列は CREATE で入っている）壊れない
	fresh, _ := OpenSQLite(filepath.Join(t.TempDir(), "fresh.db"))
	if err := Migrate(fresh, migrations); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRefusesNewerDB(t *testing.T) {
	db, _ := OpenSQLite(filepath.Join(t.TempDir(), "new.db"))
	if _, err := db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db, []Migration{{Name: "t", Up: Exec("SELECT 1")}}); err == nil {
		t.Error("知らない版の DB を黙って開いてはいけない")
	}
}

func TestMigrateRollsBackFailedStep(t *testing.T) {
	db, _ := OpenSQLite(filepath.Join(t.TempDir(), "rb.db"))
	migrations := []Migration{
		{Name: "ok", Up: Exec("CREATE TABLE a (id INTEGER)")},
		{Name: "bad", Up: Exec("CREATE TABLE b (id INTEGER)", "THIS IS NOT SQL")},
	}
	if err := Migrate(db, migrations); err == nil {
		t.Fatal("壊れた段が通っている")
	}
	v, _ := UserVersion(db)
	if v != 1 {
		t.Errorf("版 = %d, want 1（失敗した段は進まない）", v)
	}
	if _, err := db.Exec("SELECT * FROM b"); err == nil {
		t.Error("失敗した段の途中の表が残っている（巻き戻っていない）")
	}
}
