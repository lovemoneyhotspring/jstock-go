package repo

import (
	"fmt"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func newRequest(t *testing.T, id, symbol string, side domain.Side, qty int64) domain.OrderRequest {
	t.Helper()
	limit := decimal.NewFromInt(1000)
	req, err := domain.NewOrderRequest(id, symbol, side, domain.OrderTypeLimit,
		decimal.NewFromInt(qty), &limit, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func startRun(t *testing.T, r *Repo, runID string) {
	t.Helper()
	if err := r.StartRun(runID, "2026-09-14", "uat", "live"); err != nil {
		t.Fatal(err)
	}
}

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func ptr(d decimal.Decimal) *decimal.Decimal { return &d }

// --- ストップ -----------------------------------------------------------

func TestStopsRoundTripKeepsTrailingPct(t *testing.T) {
	r := openTemp(t)
	rec := StopRecord{
		Symbol: "7203", StopPrice: dec("1900"), EntryPrice: dec("2000"), CreatedOn: "2026-09-01",
		Trailing: true, ATRMultiple: dec("2.5"), TrailingPct: ptr(dec("0.08")),
		HighestClose: ptr(dec("2100")), InitialStopPrice: ptr(dec("1900")), InitialQuantity: ptr(dec("100")),
		ScaledOut: true,
	}
	if err := r.SyncStops(map[string]StopRecord{rec.Symbol: rec}); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetStops()
	if err != nil {
		t.Fatal(err)
	}
	st, ok := got["7203"]
	if !ok {
		t.Fatalf("保存したストップが無い: %+v", got)
	}
	if st.TrailingPct == nil || !st.TrailingPct.Equal(dec("0.08")) {
		t.Errorf("trailing_pct が往復していない: %v", st.TrailingPct)
	}
	if !st.StopPrice.Equal(dec("1900")) || !st.ATRMultiple.Equal(dec("2.5")) || !st.Trailing || !st.ScaledOut {
		t.Errorf("中身が違う: %+v", st)
	}
	if st.HighestClose == nil || !st.HighestClose.Equal(dec("2100")) || st.InitialQuantity == nil || !st.InitialQuantity.Equal(dec("100")) {
		t.Errorf("任意項目が往復していない: %+v", st)
	}

	// nil の trailing_pct も往復する（ATR 追従に戻る）
	rec.TrailingPct = nil
	if err := r.SyncStops(map[string]StopRecord{rec.Symbol: rec}); err != nil {
		t.Fatal(err)
	}
	got, _ = r.GetStops()
	if got["7203"].TrailingPct != nil {
		t.Errorf("nil が残らない: %v", got["7203"].TrailingPct)
	}

	if err := r.SyncStops(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = r.GetStops(); len(got) != 0 {
		t.Errorf("消えていない: %+v", got)
	}
}

// TestSyncStopsRemovesClosedPositions は、手仕舞った銘柄のストップが台帳に残らないこと。
// 残ると次に同じ銘柄を建てたとき古い建値・作成日を引き継ぐ。
func TestSyncStopsRemovesClosedPositions(t *testing.T) {
	r := openTemp(t)
	stops := map[string]StopRecord{}
	for _, sym := range []string{"7203", "6758"} {
		stops[sym] = StopRecord{Symbol: sym, StopPrice: dec("100"), EntryPrice: dec("110"),
			CreatedOn: "2026-08-01", ATRMultiple: dec("2")}
	}
	if err := r.SyncStops(stops); err != nil {
		t.Fatal(err)
	}
	// 6758 は手仕舞い済み、9984 は新規、7203 はストップが上がった
	if err := r.SyncStops(map[string]StopRecord{
		"7203": {Symbol: "7203", StopPrice: dec("105"), EntryPrice: dec("110"), CreatedOn: "2026-08-01", ATRMultiple: dec("2")},
		"9984": {Symbol: "9984", StopPrice: dec("50"), EntryPrice: dec("60"), CreatedOn: "2026-09-14", ATRMultiple: dec("2")},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetStops()
	if err != nil {
		t.Fatal(err)
	}
	if _, stale := got["6758"]; stale {
		t.Error("手仕舞った銘柄のストップが残っている")
	}
	if !got["7203"].StopPrice.Equal(dec("105")) {
		t.Errorf("更新されていない: %s", got["7203"].StopPrice)
	}
	if got["9984"].CreatedOn != "2026-09-14" {
		t.Errorf("新規が入っていない: %+v", got["9984"])
	}

	// 空にすれば全部消える
	if err := r.SyncStops(nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = r.GetStops(); len(got) != 0 {
		t.Errorf("空に揃っていない: %+v", got)
	}
}

// TestGetStopsRejectsCorruptNumber は、壊れた stop_price を 0 円として読まないこと。
func TestGetStopsRejectsCorruptNumber(t *testing.T) {
	r := openTemp(t)
	if _, err := r.db.Exec(`INSERT INTO stops (symbol, stop_price, entry_price, created_on)
		VALUES ('7203', 'abc', '2000', '2026-09-01');`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetStops(); err == nil {
		t.Error("壊れた数値を黙って 0 にしてはいけない")
	}
}

// --- WasPlaced -----------------------------------------------------------

func TestWasPlacedIgnoresRejectedUnsentAndDryRun(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	req := newRequest(t, "cid-1", "7203", domain.SideBuy, 100)

	placed, err := r.WasPlaced(req.ClientOrderID)
	if err != nil || placed {
		t.Fatalf("未記録: placed=%v err=%v", placed, err)
	}
	if err := r.RecordOrder("run-1", req, string(domain.OrderStatusPending), nil); err != nil {
		t.Fatal(err)
	}
	if placed, err = r.WasPlaced(req.ClientOrderID); err != nil || !placed {
		t.Errorf("PENDING は発注済み: placed=%v err=%v", placed, err)
	}
	for _, status := range []domain.OrderStatus{domain.OrderStatusRejected, domain.OrderStatusUnsent} {
		if err := r.UpdateOrder(req.ClientOrderID, status, decimal.Zero, nil, nil); err != nil {
			t.Fatal(err)
		}
		if placed, err = r.WasPlaced(req.ClientOrderID); err != nil || placed {
			t.Errorf("%s は送り直せる: placed=%v err=%v", status, placed, err)
		}
	}
}

// TestWasPlacedByStatus は状態ごとの WasPlaced。拒否・未送信・dry-run だけが「出していない」。
// 取消・失効は約定 0 株でも「出した」（daytrade とは違う。理由は WasPlaced のコメント）。
func TestWasPlacedByStatus(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	cases := []struct {
		status string
		filled string
		want   bool
	}{
		{string(domain.OrderStatusPending), "0", true},
		{string(domain.OrderStatusSubmitted), "0", true},
		{string(domain.OrderStatusPartiallyFilled), "50", true},
		{string(domain.OrderStatusFilled), "100", true},
		{string(domain.OrderStatusUnknown), "0", true},
		{string(domain.OrderStatusCancelled), "0", true},
		{string(domain.OrderStatusCancelled), "50", true}, // 一部約定してから取消
		{string(domain.OrderStatusExpired), "0", true},
		{string(domain.OrderStatusRejected), "0", false},
		{string(domain.OrderStatusUnsent), "0", false},
		{"dry_run", "0", false},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s_filled%s", tc.status, tc.filled), func(t *testing.T) {
			req := newRequest(t, fmt.Sprintf("cid-%d", i), "7203", domain.SideBuy, 100)
			if err := r.RecordOrder("run-1", req, tc.status, nil); err != nil {
				t.Fatal(err)
			}
			if tc.filled != "0" {
				if err := r.UpdateOrder(req.ClientOrderID, domain.OrderStatus(tc.status), dec(tc.filled), nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			placed, err := r.WasPlaced(req.ClientOrderID)
			if err != nil {
				t.Fatalf("台帳を読めません: %v", err)
			}
			if placed != tc.want {
				t.Errorf("WasPlaced(%s, 約定 %s 株) = %v, want %v", tc.status, tc.filled, placed, tc.want)
			}
		})
	}
}

// TestWasPlacedFailsClosedOnDBError は、台帳が読めないときに「未発注」と答えないこと。
func TestWasPlacedFailsClosedOnDBError(t *testing.T) {
	r := openTemp(t)
	_ = r.Close()
	placed, err := r.WasPlaced("cid-1")
	if err == nil {
		t.Fatal("閉じた台帳でエラーにならない（二重発注の柵が外れる）")
	}
	if placed {
		t.Error("エラー時に発注済み扱いにもしない")
	}
}

// --- RecordOrder ------------------------------------------------------------

// TestRecordOrderKeepsFillsOnConflict は、同じ ID の書き直しが約定の記録を消さないこと。
func TestRecordOrderKeepsFillsOnConflict(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	startRun(t, r, "run-2")
	req := newRequest(t, "cid-1", "7203", domain.SideBuy, 100)
	if err := r.RecordOrder("run-1", req, string(domain.OrderStatusPending), nil); err != nil {
		t.Fatal(err)
	}
	brokerID := "N/1"
	price := dec("1010")
	if err := r.UpdateOrder(req.ClientOrderID, domain.OrderStatusFilled, dec("100"), &price, &brokerID); err != nil {
		t.Fatal(err)
	}

	// 同じ ID で書き直す（現実には REJECTED / UNSENT / dry_run の行に対して起きる）
	if err := r.RecordOrder("run-2", req, string(domain.OrderStatusPending), nil); err != nil {
		t.Fatal(err)
	}
	got, err := r.GetOrder(req.ClientOrderID)
	if err != nil || got == nil {
		t.Fatalf("引けない: %v %v", got, err)
	}
	if !got.FilledQuantity.Equal(dec("100")) || got.AvgFillPrice == nil || !got.AvgFillPrice.Equal(price) {
		t.Errorf("約定の記録が巻き戻った: filled=%s avg=%v", got.FilledQuantity, got.AvgFillPrice)
	}
	if got.RunID != "run-2" || got.Status != domain.OrderStatusPending {
		t.Errorf("発注の記録は書き換わるはず: %+v", got)
	}
}

// TestOrdersTodayCountsByJSTDate は、08:40 JST（前日 23:40 UTC）の注文がその JST の日に数えられること。
func TestOrdersTodayCountsByJSTDate(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	at := time.Date(2026, 9, 13, 23, 40, 0, 0, time.UTC) // 2026-09-14 08:40 JST
	req := newRequest(t, "cid-1", "7203", domain.SideBuy, 100)
	if err := r.recordOrderAt("run-1", req, string(domain.OrderStatusSubmitted), nil, at); err != nil {
		t.Fatal(err)
	}
	if n, err := r.OrdersToday("2026-09-14"); err != nil || n != 1 {
		t.Errorf("JST の当日に数えられていない: n=%d err=%v", n, err)
	}
	if n, _ := r.OrdersToday("2026-09-13"); n != 0 {
		t.Errorf("UTC の日付で数えている: n=%d", n)
	}

	// 当日買付も同じ日付で引く
	if err := r.UpdateOrder(req.ClientOrderID, domain.OrderStatusFilled, dec("100"), nil, nil); err != nil {
		t.Fatal(err)
	}
	bought, err := r.BoughtToday("2026-09-14")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bought["7203"]; !ok {
		t.Errorf("当日買付に無い: %v", bought)
	}

	// dry-run は数えない
	dry := newRequest(t, "cid-2", "6758", domain.SideBuy, 100)
	if err := r.recordOrderAt("run-1", dry, "dry_run", nil, at); err != nil {
		t.Fatal(err)
	}
	if n, _ := r.OrdersToday("2026-09-14"); n != 1 {
		t.Errorf("dry-run を数えている: n=%d", n)
	}
}

// TestPlacedOnBackfillUsesJST は、旧い行（placed_on が無い）の埋め直しが JST の日付になること。
func TestPlacedOnBackfillUsesJST(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	if _, err := r.db.Exec(`INSERT INTO orders (client_order_id, run_id, symbol, side, order_type, quantity, status, placed_at)
		VALUES ('old', 'run-1', '7203', 'BUY', 'LIMIT', '100', 'SUBMITTED', '2026-09-13T23:40:00Z');`); err != nil {
		t.Fatal(err)
	}
	// 版を 1 段戻して開き直すと、placed_on の段がもう一度走る
	if _, err := r.db.Exec("PRAGMA user_version = 1;"); err != nil {
		t.Fatal(err)
	}
	path := dbPathOf(t, r)
	_ = r.Close()
	reopened, err := OpenRepo(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if n, err := reopened.OrdersToday("2026-09-14"); err != nil || n != 1 {
		t.Errorf("埋め直した placed_on が JST になっていない: n=%d err=%v", n, err)
	}
}

func dbPathOf(t *testing.T, r *Repo) string {
	t.Helper()
	var seq int
	var name, path string
	if err := r.db.QueryRow("PRAGMA database_list;").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- 注文の読み書き ------------------------------------------------------------

func TestUnresolvedOrdersRejectsCorruptQuantity(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	if _, err := r.db.Exec(`INSERT INTO orders (client_order_id, run_id, symbol, side, order_type, quantity, status, placed_at, placed_on)
		VALUES ('bad', 'run-1', '7203', 'BUY', 'LIMIT', 'abc', 'SUBMITTED', '2026-09-14T00:00:00Z', '2026-09-14');`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UnresolvedOrders(); err == nil {
		t.Error("壊れた数量を 0 株として読んではいけない")
	}
}

// TestCancelledAfterPartialFillKeepsFills は、部分約定の後に取り消された注文。本番の同期は
// 取消でも証券会社の約定数量ごと UpdateOrder で書くので、約定分が残り未確定にも出ない。
func TestCancelledAfterPartialFillKeepsFills(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	req := newRequest(t, "cid-1", "7203", domain.SideBuy, 200)
	if err := r.RecordOrder("run-1", req, string(domain.OrderStatusSubmitted), nil); err != nil {
		t.Fatal(err)
	}
	price := dec("1000")
	if err := r.UpdateOrder(req.ClientOrderID, domain.OrderStatusPartiallyFilled, dec("100"), &price, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateOrder(req.ClientOrderID, domain.OrderStatusCancelled, dec("100"), nil, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := r.GetOrder(req.ClientOrderID)
	if got.Status != domain.OrderStatusCancelled || !got.FilledQuantity.Equal(dec("100")) {
		t.Errorf("取消で約定分が消えた: %+v", got)
	}
	// 終了状態なので未確定には出ない
	open, err := r.UnresolvedOrders()
	if err != nil || len(open) != 0 {
		t.Errorf("取消済みが未確定に残る: %+v err=%v", open, err)
	}
	if missing, err := r.GetOrder("いない"); err != nil || missing != nil {
		t.Errorf("無い注文は nil: %v %v", missing, err)
	}
}
