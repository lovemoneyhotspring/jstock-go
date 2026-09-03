package storage

import (
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
