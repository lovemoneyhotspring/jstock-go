package storage

import (
	"strings"
	"testing"
)

func TestDuckDB(t *testing.T) {
	db, err := OpenDuckDB()
	if err != nil {
		t.Fatalf("failed to open duckdb: %v", err)
	}
	defer db.Close()

	results, err := QueryDuckDB(db, "SELECT 42 AS answer, 'hello' AS msg")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 row, got %d", len(results))
	}
	if results[0]["msg"] != "hello" {
		t.Errorf("expected 'hello', got %v", results[0]["msg"])
	}
}

func TestDuckDBMemoryLimitDefault(t *testing.T) {
	t.Setenv(DuckDBMemoryLimitEnv, "")
	got, err := DuckDBMemoryLimit()
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultDuckDBMemoryLimit {
		t.Errorf("既定 = %q, want %q", got, DefaultDuckDBMemoryLimit)
	}
}

func TestDuckDBMemoryLimitRejectsBadFormat(t *testing.T) {
	// DSN に埋めるので、& で別の設定を紛れ込ませられないこと
	for _, bad := range []string{"4GB&threads=64", "たくさん", "4", "-1GB", "4 GB extra"} {
		t.Setenv(DuckDBMemoryLimitEnv, bad)
		if _, err := DuckDBMemoryLimit(); err == nil {
			t.Errorf("%q を通してしまった", bad)
		}
	}
}

func TestDuckDBMemoryLimitAccepts(t *testing.T) {
	for _, ok := range []string{"4GB", "512MB", "1.5GB", "2GiB", "800B"} {
		t.Setenv(DuckDBMemoryLimitEnv, ok)
		got, err := DuckDBMemoryLimit()
		if err != nil {
			t.Errorf("%q を弾いてしまった: %v", ok, err)
			continue
		}
		if got != ok {
			t.Errorf("%q → %q", ok, got)
		}
	}
}

// 上限が実際に DuckDB へ効いているか（渡さないとシステムメモリの 80% になる）
func TestOpenDuckDBAppliesLimit(t *testing.T) {
	t.Setenv(DuckDBMemoryLimitEnv, "512MB")
	db, err := OpenDuckDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var limit string
	if err := db.QueryRow("SELECT current_setting('memory_limit')").Scan(&limit); err != nil {
		t.Fatal(err)
	}
	// DuckDB は 512MB を "488.2 MiB" と表示する
	if !strings.Contains(limit, "MiB") {
		t.Errorf("memory_limit = %q（上限が効いていない）", limit)
	}
}
