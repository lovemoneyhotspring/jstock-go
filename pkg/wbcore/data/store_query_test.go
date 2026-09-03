package data

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestReadManyAndLastDate(t *testing.T) {
	store := NewBarStore(t.TempDir())
	if err := store.Write("7203", []domain.Bar{
		bar("7203", "2026-09-01", 100),
		bar("7203", "2026-09-02", 101),
	}); err != nil {
		t.Fatal(err)
	}

	many, err := store.ReadMany([]string{"7203", "9999"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(many) != 1 || len(many["7203"]) != 2 {
		t.Fatalf("ReadMany = %v（未保存の銘柄は含めない）", many)
	}

	last, err := store.LastDate("7203")
	if err != nil || last != "2026-09-02" {
		t.Fatalf("LastDate = %q, %v", last, err)
	}
	if last, err := store.LastDate("9999"); err != nil || last != "" {
		t.Fatalf("未保存の LastDate = %q, %v", last, err)
	}
}

func TestUpsertMergesLastWins(t *testing.T) {
	store := NewBarStore(t.TempDir())
	if err := store.Write("7203", []domain.Bar{
		bar("7203", "2026-09-01", 100),
		bar("7203", "2026-09-02", 101),
	}); err != nil {
		t.Fatal(err)
	}

	total, err := store.Upsert("7203", []domain.Bar{
		bar("7203", "2026-09-02", 999), // 訂正された足
		bar("7203", "2026-09-03", 102),
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("総本数 = %d, want 3", total)
	}
	bars, err := store.Read("7203", "2026-09-02", "2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	if !bars[0].Close.Equal(decimal.NewFromInt(999)) {
		t.Errorf("後勝ちになっていない: %s", bars[0].Close)
	}

	// 空の追加は既存を壊さない
	total, err = store.Upsert("7203", nil)
	if err != nil || total != 3 {
		t.Fatalf("空の Upsert = %d, %v", total, err)
	}
}

func TestQueryAndSummary(t *testing.T) {
	store := NewBarStore(t.TempDir())

	// 1 銘柄も無いときは空の表（DuckDB を呼ばない）
	empty, err := store.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Height() != 0 {
		t.Fatalf("空のはずが %d 行", empty.Height())
	}

	if err := store.Write("7203", []domain.Bar{
		bar("7203", "2026-09-01", 100),
		bar("7203", "2026-09-02", 101),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("6758", []domain.Bar{bar("6758", "2026-09-01", 50)}); err != nil {
		t.Fatal(err)
	}

	frame, err := store.Query("SELECT symbol, count(*) AS n FROM bars GROUP BY symbol ORDER BY symbol")
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d", frame.Height())
	}
	if frame.Rows[0]["symbol"] != "6758" || frame.Rows[0]["n"] != int64(1) {
		t.Fatalf("先頭行 = %v", frame.Rows[0])
	}

	summary, err := store.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Height() != 2 {
		t.Fatalf("要約の行数 = %d", summary.Height())
	}
	row := summary.Rows[1]
	if row["symbol"] != "7203" || row["bars"] != int64(2) {
		t.Fatalf("7203 の要約 = %v", row)
	}
	if row["first"] != "2026-09-01" || row["last"] != "2026-09-02" {
		t.Fatalf("期間 = %v 〜 %v", row["first"], row["last"])
	}
}
