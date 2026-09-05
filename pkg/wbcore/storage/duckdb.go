package storage

import (
	"database/sql"
	"fmt"
	"os"
	"regexp"
	"strings"

	_ "github.com/marcboeker/go-duckdb"
)

const (
	// DuckDBMemoryLimitEnv は DuckDB のメモリ上限を変える環境変数（"4GB" / "512MB"）。
	DuckDBMemoryLimitEnv = "WBJP_DUCKDB_MEMORY_LIMIT"
	// DefaultDuckDBMemoryLimit は既定の上限。
	//
	// DuckDB の既定は「システムメモリの 80%」（このサーバーでは 9.3GiB）で、しかも
	// **Go のヒープの外**なので GOMEMLIMIT も JQUANTS_READ_BUDGET_MB も届かない。
	// 上限を超えたら DuckDB は一時ファイルにスピルするので、決めても落ちはしない（遅くなるだけ）。
	//
	// 値はいちばん重い daytrade backtest（10 年 × 4,000 銘柄のパネルを 1 本の SQL で組む）
	// の実測で決めた。損益はどれも同一で、メモリと時間のつり合いだけが動く:
	//
	//	上限      プロセス全体のピーク   時間
	//	無設定    6.56GB                31.8 秒
	//	4GB       5.95GB                31.6 秒
	//	3GB       4.16GB                26.9 秒  ← 既定
	//	2GB       3.86GB                35.0 秒
	//	1GB       2.61GB                38.8 秒
	//
	// Go 側（パネルを構造体に持つ）が約 1.6GB あるので、プロセス全体はこの上限 + 1.6GB
	// と見ておく。時間は実行ごとに数秒ぶれるので、上限との関係は「絞るほどスピルして遅くなる」
	// という傾向として読む。手元で速く回したいときは WBJP_DUCKDB_MEMORY_LIMIT で上げる。
	DefaultDuckDBMemoryLimit = "3GB"
)

// memoryLimitPattern は DuckDB が受ける大きさの書式（10GB / 512MiB / 1.5GB）。
var memoryLimitPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?\s*(B|[KMGT]B|[KMGT]iB)$`)

// DuckDBMemoryLimit は DuckDB に渡すメモリ上限。
func DuckDBMemoryLimit() (string, error) {
	limit := strings.TrimSpace(os.Getenv(DuckDBMemoryLimitEnv))
	if limit == "" {
		return DefaultDuckDBMemoryLimit, nil
	}
	// DSN に埋めるので、書式を確かめてから通す（& を含む値で別の設定を紛れ込ませない）
	if !memoryLimitPattern.MatchString(limit) {
		return "", fmt.Errorf("%s の書式が違います: %q（例: 4GB / 512MB）", DuckDBMemoryLimitEnv, limit)
	}
	return limit, nil
}

// OpenDuckDB はインメモリの DuckDB 接続を開く。メモリ上限を必ず渡す（上記の理由）。
func OpenDuckDB() (*sql.DB, error) {
	limit, err := DuckDBMemoryLimit()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("duckdb", "?memory_limit="+limit)
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
