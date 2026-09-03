package storage

import (
	"database/sql"
	"fmt"
)

// Migration はスキーマの 1 段。Up は表の作成や列の追加をトランザクションの中で行う。
//
// 既存の DB は「版 0 で表だけある」状態で来るので、段の中身は冪等に書く
// （CREATE TABLE IF NOT EXISTS / AddColumn）。版の番号だけを進めて、
// 次に開いたときにその段を飛ばせるようにする。
type Migration struct {
	Name string
	Up   func(tx *sql.Tx) error
}

// Migrate は PRAGMA user_version を版として、未適用の段だけを順に適用する。
// 各段は 1 トランザクション。途中で失敗した段は巻き戻り、版は進まない。
//
// 4 つの台帳（wbjp / accum / daytrade / jquants）が同じ仕組みを使う。
// 台帳ごとに列の有無を PRAGMA table_info で探る書き方が 3 通りあると、
// 列を足すたびに 3 通りの書き方を考えることになる。
func Migrate(db *sql.DB, migrations []Migration) error {
	current, err := UserVersion(db)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("DB の版 %d がこの実行ファイルの知る版 %d より新しい（古い実行ファイルで開いている）",
			current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		m := migrations[i]
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("マイグレーション %q を始められません: %w", m.Name, err)
		}
		if err := m.Up(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("マイグレーション %q に失敗しました: %w", m.Name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("マイグレーション %q の版を進められません: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("マイグレーション %q を確定できません: %w", m.Name, err)
		}
	}
	return nil
}

// UserVersion は PRAGMA user_version（適用済みの段の数）。
func UserVersion(db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, fmt.Errorf("DB の版を読めません: %w", err)
	}
	return version, nil
}

// Exec は SQL を順に流すだけの段。
func Exec(statements ...string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
		return nil
	}
}

// AddColumn は列が無ければ足す。ALTER TABLE ADD COLUMN は冪等でないので、
// 既に列がある DB（新しい CREATE TABLE で作られた）では何もしない。
func AddColumn(tx *sql.Tx, table, column, definition string) error {
	has, err := hasColumn(tx, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	if err != nil {
		return fmt.Errorf("%s に %s 列を足せません: %w", table, column, err)
	}
	return nil
}

// AddColumns は複数の列を AddColumn で足す段。
func AddColumns(table string, columns map[string]string) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		for column, definition := range columns {
			if err := AddColumn(tx, table, column, definition); err != nil {
				return err
			}
		}
		return nil
	}
}

func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt *string
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}
