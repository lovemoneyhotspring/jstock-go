package repo

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TestExpectedHoldings は建玉の照会が 0 件のときに「持っているはず」とする銘柄の再現
// （2026-09-24 の再点検）。
func TestExpectedHoldings(t *testing.T) {
	r := openTemp(t)
	// 前に成功した発注する回は prev（09-22）。失敗した回・dry-run・別の env は基準にしない
	if _, err := r.db.Exec(`INSERT INTO runs (run_id, started_at, as_of, env, mode, status) VALUES
		('old', '2026-09-19T00:00:00Z', '2026-09-19', 'prod', 'live', 'success'),
		('prev', '2026-09-22T00:00:00Z', '2026-09-22', 'prod', 'live', 'success'),
		('failed', '2026-09-23T00:00:00Z', '2026-09-23', 'prod', 'live', 'failed'),
		('dry', '2026-09-23T01:00:00Z', '2026-09-23', 'prod', 'dry_run', 'success'),
		('uat', '2026-09-23T02:00:00Z', '2026-09-23', 'uat', 'live', 'success'),
		('now', '2026-09-24T00:00:00Z', '2026-09-24', 'prod', 'live', 'running');`); err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 9, 19, 0, 1, 0, 0, time.UTC)
	after := time.Date(2026, 9, 22, 0, 1, 0, 0, time.UTC)
	order := func(id, sym string, side domain.Side, at time.Time, status domain.OrderStatus, filled string) {
		t.Helper()
		if err := r.recordOrderAt("prev", newRequest(t, id, sym, side, 100), string(status), nil, at); err != nil {
			t.Fatal(err)
		}
		if err := r.UpdateOrder(id, status, dec(filled), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// ストップがあり、売っていない → 持っているはず
	for _, sym := range []string{"1001", "1002", "1003"} {
		if err := r.SaveStop(StopRecord{Symbol: sym, StopPrice: dec("900"), EntryPrice: dec("1000"),
			CreatedOn: "2026-09-10", ATRMultiple: dec("2")}); err != nil {
			t.Fatal(err)
		}
	}
	order("s2", "1002", domain.SideSell, after, domain.OrderStatusFilled, "100")  // 前の回の売りが約定 → 外す
	order("s3", "1003", domain.SideSell, after, domain.OrderStatusExpired, "0")   // 売りが失効 → 残す
	order("b4", "1004", domain.SideBuy, after, domain.OrderStatusFilled, "100")   // 前の回の買いが約定 → 持っているはず
	order("b5", "1005", domain.SideBuy, after, domain.OrderStatusExpired, "0")    // 買いが失効 → 持っていない
	order("b6", "1006", domain.SideBuy, after, domain.OrderStatusSubmitted, "0")  // 約定が分からない → 持っているはず
	order("b7", "1007", domain.SideBuy, before, domain.OrderStatusFilled, "100")  // 前の回より前（ストップが無い＝売れた）→ 見ない
	order("b8", "1008", domain.SideBuy, after, domain.OrderStatusRejected, "0")   // 拒否 → 持っていない
	order("b9", "1009", domain.SideBuy, after, domain.OrderStatusFilled, "100")   // 買って
	order("s9", "1009", domain.SideSell, after, domain.OrderStatusSubmitted, "0") // 売りの約定が分からない → 外す

	got, err := r.ExpectedHoldings("prod", "now")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1001", "1003", "1004", "1006"}
	if len(got) != len(want) {
		t.Fatalf("持っているはずの銘柄: %v（期待 %v）", got, want)
	}
	for _, sym := range want {
		if _, ok := got[sym]; !ok {
			t.Errorf("%s が持っているはずに入らない: %v", sym, got)
		}
	}

	// 成功した発注する回が無ければ全期間を見る（1007 の古い買いも数える）
	got, err = r.ExpectedHoldings("dev", "now")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["1007"]; !ok {
		t.Errorf("前の回が無いのに古い買いを数えない: %v", got)
	}
}
