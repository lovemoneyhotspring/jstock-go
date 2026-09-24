package storage

import (
	"context"
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
	Up   func(tx Tx) error
}

// Tx は段が使える操作。BEGIN IMMEDIATE で始めた接続（Migrate が渡す）と *sql.Tx の両方が満たす。
type Tx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// connTx は BEGIN IMMEDIATE を流した接続を Tx として段に渡す。
// database/sql の Begin は DEFERRED でしか始められないので、接続を固定して自分で BEGIN する。
type connTx struct {
	ctx  context.Context
	conn *sql.Conn
}

func (c connTx) Exec(query string, args ...any) (sql.Result, error) {
	return c.conn.ExecContext(c.ctx, query, args...)
}

func (c connTx) Query(query string, args ...any) (*sql.Rows, error) {
	return c.conn.QueryContext(c.ctx, query, args...)
}

func (c connTx) QueryRow(query string, args ...any) *sql.Row {
	return c.conn.QueryRowContext(c.ctx, query, args...)
}

// Migrate は PRAGMA user_version を版として、未適用の段だけを順に適用する。
// 各段は 1 トランザクション。途中で失敗した段は巻き戻り、版は進まない。
//
// 4 つの台帳（wbjp / accum / daytrade / jquants）が同じ仕組みを使う。
// 台帳ごとに列の有無を PRAGMA table_info で探る書き方が 3 通りあると、
// 列を足すたびに 3 通りの書き方を考えることになる。
//
// 同じ DB を 2 つのプロセスが同時に開いても段を二重に当てないよう、各段は
// BEGIN IMMEDIATE（最初から書き込みの錠を取る）で始め、版は**錠を取ってから**読み直す。
// DEFERRED で始めて tx の外で読んだ版を信じると、両方が同じ段を当てにいき、後の方は
// 読みの錠から書きの錠へ上がれずに SQLITE_BUSY で落ちる（busy_timeout も効かない）。
func Migrate(db *sql.DB, migrations []Migration) error {
	// 版が最新なら書き込みの錠を取らずに帰る（毎回の起動で他の書き手を待たせない）
	current, err := UserVersion(db)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return newerDBError(current, len(migrations))
	}
	if current == len(migrations) {
		return nil
	}

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("マイグレーションの接続を取れません: %w", err)
	}
	defer conn.Close()
	tx := connTx{ctx: ctx, conn: conn}
	for {
		done, err := migrateStep(tx, migrations)
		if err != nil || done {
			return err
		}
	}
}

// migrateStep は錠を取って版を読み直し、次の 1 段を当てる。当てる段が無ければ done。
func migrateStep(tx connTx, migrations []Migration) (done bool, err error) {
	if _, err := tx.Exec("BEGIN IMMEDIATE"); err != nil {
		return false, fmt.Errorf("マイグレーションを始められません: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// 段の失敗で SQLite が既に巻き戻していることもある（その ROLLBACK の失敗は無視）
			_, _ = tx.Exec("ROLLBACK")
		}
	}()
	var current int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return false, fmt.Errorf("DB の版を読めません: %w", err)
	}
	if current > len(migrations) {
		return false, newerDBError(current, len(migrations))
	}
	if current == len(migrations) {
		return true, nil // 別のプロセスが先に当て終えた
	}
	m := migrations[current]
	if err := m.Up(tx); err != nil {
		return false, fmt.Errorf("マイグレーション %q に失敗しました: %w", m.Name, err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", current+1)); err != nil {
		return false, fmt.Errorf("マイグレーション %q の版を進められません: %w", m.Name, err)
	}
	if _, err := tx.Exec("COMMIT"); err != nil {
		return false, fmt.Errorf("マイグレーション %q を確定できません: %w", m.Name, err)
	}
	committed = true
	return current+1 == len(migrations), nil
}

func newerDBError(current, known int) error {
	return fmt.Errorf("DB の版 %d がこの実行ファイルの知る版 %d より新しい（古い実行ファイルで開いている）",
		current, known)
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
func Exec(statements ...string) func(tx Tx) error {
	return func(tx Tx) error {
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
func AddColumn(tx Tx, table, column, definition string) error {
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
func AddColumns(table string, columns map[string]string) func(tx Tx) error {
	return func(tx Tx) error {
		for column, definition := range columns {
			if err := AddColumn(tx, table, column, definition); err != nil {
				return err
			}
		}
		return nil
	}
}

func hasColumn(tx Tx, table, column string) (bool, error) {
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
