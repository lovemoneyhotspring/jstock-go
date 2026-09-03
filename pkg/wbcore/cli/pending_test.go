package cli

import (
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

func TestResolvePendingOrder(t *testing.T) {
	db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE orders (client_order_id TEXT PRIMARY KEY, broker_order_id TEXT, symbol TEXT,
		quantity TEXT, filled_quantity TEXT DEFAULT '0', status TEXT, avg_fill_price TEXT, placed_at TEXT, updated_at TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.Exec("INSERT INTO orders (client_order_id, symbol, quantity, status, placed_at) VALUES (?, '7203', '100', 'PENDING', '2026-09-04T00:00:00Z')", id); err != nil {
			t.Fatal(err)
		}
	}
	pending, _ := ListPending(db)
	if len(pending) != 3 {
		t.Fatalf("pending=%d", len(pending))
	}
	if _, err := ResolvePendingOrder(db, "a", Fix{Status: domain.OrderStatusUnsent}); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePendingOrder(db, "b", Fix{Status: domain.OrderStatusFilled, BrokerOrderID: "9/20260904", Filled: "100", Price: "1234"}); err != nil {
		t.Fatal(err)
	}
	// 既に確定した行は触らない
	if _, err := ResolvePendingOrder(db, "a", Fix{Status: domain.OrderStatusFilled, BrokerOrderID: "x"}); err == nil {
		t.Error("UNSENT を上書きしてはいけない")
	}
	if _, err := ResolvePendingOrder(db, "zzz", Fix{Status: domain.OrderStatusUnsent}); err == nil {
		t.Error("無い ID はエラー")
	}
	var status, bid, filled string
	if err := db.QueryRow("SELECT status, broker_order_id, filled_quantity FROM orders WHERE client_order_id = 'b'").Scan(&status, &bid, &filled); err != nil {
		t.Fatal(err)
	}
	if status != "FILLED" || bid != "9/20260904" || filled != "100" {
		t.Errorf("b: %s %s %s", status, bid, filled)
	}
	pending, _ = ListPending(db)
	if len(pending) != 1 || pending[0].ClientOrderID != "c" {
		t.Errorf("残り: %+v", pending)
	}
}
