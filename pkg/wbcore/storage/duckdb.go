package storage

import (
	"database/sql"
	"fmt"

	_ "github.com/marcboeker/go-duckdb"
)

// OpenDuckDB はインメモリの DuckDB 接続を開く。
func OpenDuckDB() (*sql.DB, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("failed to open duckdb: %w", err)
	}
	return db, nil
}

// QueryDuckDB はアドホックな SQL を DuckDB で実行し、結果をマップのスライスとして返す。
func QueryDuckDB(db *sql.DB, query string) ([]map[string]any, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("duckdb query error: %w", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	for rows.Next() {
		colVals := make([]any, len(cols))
		colPointers := make([]any, len(cols))
		for i := range colVals {
			colPointers[i] = &colVals[i]
		}

		if err := rows.Scan(colPointers...); err != nil {
			return nil, err
		}

		rowMap := make(map[string]any, len(cols))
		for i, col := range cols {
			val := colVals[i]
			if b, ok := val.([]byte); ok {
				val = string(b)
			}
			rowMap[col] = val
		}
		results = append(results, rowMap)
	}

	return results, rows.Err()
}
