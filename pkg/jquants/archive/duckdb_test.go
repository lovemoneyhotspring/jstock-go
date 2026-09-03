package archive

import (
	"fmt"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

// TestDuckDBReadsWrittenParquet は書いた Parquet を DuckDB が読めることを確かめる
// （query コマンドが実データを引けるかの担保）。
func TestDuckDBReadsWrittenParquet(t *testing.T) {
	root := t.TempDir()
	a := NewArchive(root)
	ep := bars()
	f, err := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "Code": "72030", "Close": "100"},
		{"Date": "2025-01-07", "Code": "72030", "Close": "110"},
	}, ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Upsert(ep, f); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenDuckDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	glob := a.Directory(ep) + "/*.parquet"
	rows, err := storage.QueryDuckDB(db, fmt.Sprintf(
		"SELECT Code, Date, Close FROM read_parquet('%s', union_by_name=true) WHERE Date >= DATE '2025-01-07'", glob))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("DuckDB の結果 = %v", rows)
	}
	t.Logf("DuckDB から読めた: %v", rows[0])
}
