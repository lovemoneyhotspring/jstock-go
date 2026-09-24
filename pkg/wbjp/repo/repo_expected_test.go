package repo

import (
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TestExpectedHoldings は建玉の照会が 0 件のときに「持っているはず」とする銘柄の再現
// （2026-09-24 の再点検）。株数で差し引く: 前の回の建玉 ＋ 買い − 売りの約定。
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
	order := func(id, sym string, side domain.Side, qty int64, at time.Time, status domain.OrderStatus, filled string) {
		t.Helper()
		if err := r.recordOrderAt("prev", newRequest(t, id, sym, side, qty), string(status), nil, at); err != nil {
			t.Fatal(err)
		}
		if err := r.UpdateOrder(id, status, dec(filled), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// 前の回の建玉の記録（ストップも前の回で作った）
	var snap []domain.Position
	for sym, qty := range map[string]string{"1001": "100", "1002": "100", "1003": "100", "1010": "200", "1011": "200", "1012": "200"} {
		snap = append(snap, domain.Position{Symbol: sym, Quantity: dec(qty), CostPrice: dec("1000"), LastPrice: dec("1000")})
		if err := r.SaveStop(StopRecord{Symbol: sym, StopPrice: dec("900"), EntryPrice: dec("1000"),
			CreatedOn: "2026-09-10", ATRMultiple: dec("2")}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RecordSnapshot("prev", "2026-09-22", snap); err != nil {
		t.Fatal(err)
	}
	// 1001: ストップがあり、売っていない → 持っているはず
	order("s2", "1002", domain.SideSell, 100, after, domain.OrderStatusFilled, "100")          // 全株の売りが約定 → 外す
	order("s3", "1003", domain.SideSell, 100, after, domain.OrderStatusExpired, "0")           // 売りが失効 → 残す
	order("b4", "1004", domain.SideBuy, 100, after, domain.OrderStatusFilled, "100")           // 前の回の買いが約定 → 持っているはず
	order("b5", "1005", domain.SideBuy, 100, after, domain.OrderStatusExpired, "0")            // 買いが失効 → 持っていない
	order("b6", "1006", domain.SideBuy, 100, after, domain.OrderStatusSubmitted, "0")          // 約定が分からない → 持っているはず
	order("b7", "1007", domain.SideBuy, 100, before, domain.OrderStatusFilled, "100")          // 前の回より前（記録に無い＝売れた）→ 見ない
	order("b8", "1008", domain.SideBuy, 100, after, domain.OrderStatusRejected, "0")           // 拒否 → 持っていない
	order("b9", "1009", domain.SideBuy, 100, after, domain.OrderStatusFilled, "100")           // 買って
	order("s9", "1009", domain.SideSell, 100, after, domain.OrderStatusSubmitted, "0")         // 売りの約定が分からない → 引かない（残す）
	order("s10", "1010", domain.SideSell, 200, after, domain.OrderStatusExpired, "100")        // 指値の売りが一部約定して失効 → 100 株残る
	order("s11", "1011", domain.SideSell, 200, after, domain.OrderStatusSubmitted, "100")      // 一部約定のまま照会できない → 100 株残る
	order("s12a", "1012", domain.SideSell, 200, after, domain.OrderStatusExpired, "150")       // 2 回に分けて
	order("s12b", "1012", domain.SideSell, 50, after, domain.OrderStatusFilled, "50")          // 全株を売り切った → 外す
	order("b13", "1013", domain.SideBuy, 200, after, domain.OrderStatusPartiallyFilled, "100") // 約定途中の買いは全株を足す

	got, err := r.ExpectedHoldings("prod", "now")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1001", "1003", "1004", "1006", "1009", "1010", "1011", "1013"}
	if len(got) != len(want) {
		t.Fatalf("持っているはずの銘柄: %v（期待 %v）", got, want)
	}
	for _, sym := range want {
		if _, ok := got[sym]; !ok {
			t.Errorf("%s が持っているはずに入らない: %v", sym, got)
		}
	}
	for sym, qty := range map[string]string{"1010": "100 株", "1011": "100 株", "1013": "200 株"} {
		if !strings.Contains(got[sym], qty) {
			t.Errorf("%s の株数: %q（期待 %s）", sym, got[sym], qty)
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

// TestExpectedHoldingsStopWithoutSnapshot は、前の回の建玉の記録が無い（記録の書き込みに
// 失敗した）銘柄のストップ。株数が分からないので、売りの約定があっても外さない（止める側）。
func TestExpectedHoldingsStopWithoutSnapshot(t *testing.T) {
	r := openTemp(t)
	if _, err := r.db.Exec(`INSERT INTO runs (run_id, started_at, as_of, env, mode, status) VALUES
		('prev', '2026-09-22T00:00:00Z', '2026-09-22', 'prod', 'live', 'success'),
		('now', '2026-09-24T00:00:00Z', '2026-09-24', 'prod', 'live', 'running');`); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveStop(StopRecord{Symbol: "2001", StopPrice: dec("900"), EntryPrice: dec("1000"),
		CreatedOn: "2026-09-10", ATRMultiple: dec("2")}); err != nil {
		t.Fatal(err)
	}
	if err := r.recordOrderAt("prev", newRequest(t, "s", "2001", domain.SideSell, 100), string(domain.OrderStatusFilled), nil,
		time.Date(2026, 9, 22, 0, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateOrder("s", domain.OrderStatusFilled, dec("100"), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.ExpectedHoldings("prod", "now")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["2001"]; !ok {
		t.Errorf("株数が分からないのにストップの銘柄を外した: %v", got)
	}
}
