package rate

import (
	"context"
	"testing"
	"time"
)

func TestRecordFetchReplacesSameStamp(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 11, 7, 30, 0, 0, jst)
	if err := store.RecordFetch(ctx, at, 0, 0, "timeout", time.Second); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFetch(ctx, at, 4, 2, "ok", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	var n, rows, newRows, ms int
	var status string
	if err := store.DB().QueryRow(`SELECT COUNT(*), MAX(rows), MAX(new_rows), MAX(status), MAX(elapsed_ms) FROM fetches`).
		Scan(&n, &rows, &newRows, &status, &ms); err != nil {
		t.Fatal(err)
	}
	if n != 1 || rows != 4 || newRows != 2 || status != "ok" || ms != 1500 {
		t.Errorf("fetches = %d 行 rows=%d new=%d status=%s ms=%d", n, rows, newRows, status, ms)
	}
}

func TestSaveEventsAndImportedDays(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	events := []Event{
		{FeedDate: "2026-09-10", PubLabel: "9/9", Code: "1803", Kind: "rating", Direction: "down", NewsID: "a", NewsTime: "0710"},
		{FeedDate: "2026-09-10", PubLabel: "9/9", Code: "1803", Kind: "target", Direction: "down", NewsID: "b", NewsTime: "0710"},
	}
	if err := store.SaveEvents(ctx, "2026-09-10", events, 120); err != nil {
		t.Fatal(err)
	}
	// 取り直しても増えない
	if err := store.SaveEvents(ctx, "2026-09-10", events, 125); err != nil {
		t.Fatal(err)
	}
	// 動きの無い日も取り込んだ日として残る
	if err := store.SaveEvents(ctx, "2026-09-13", nil, 0); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM rating_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rating_events = %d 行", n)
	}
	var news, count int
	if err := store.DB().QueryRow(`SELECT news_count, event_count FROM news_days WHERE feed_date = '2026-09-10'`).
		Scan(&news, &count); err != nil {
		t.Fatal(err)
	}
	if news != 125 || count != 2 {
		t.Errorf("news_days = %d / %d", news, count)
	}
	done, err := store.ImportedDays(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 || !done["2026-09-10"] || !done["2026-09-13"] {
		t.Errorf("ImportedDays = %v", done)
	}
}

func TestSaveTradersAndRecomputeDirections(t *testing.T) {
	store := openRateStore(t)
	ctx := context.Background()
	entries, err := ParseTraders(tradersSample, "200501")
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.SaveTraders(ctx, "200501", entries)
	if err != nil || added != 3 {
		t.Fatalf("added = %d err = %v", added, err)
	}
	if added, err = store.SaveTraders(ctx, "200501", entries); err != nil || added != 0 {
		t.Fatalf("2 回目 added = %d err = %v", added, err)
	}
	direction := func(code string) string {
		t.Helper()
		var d string
		if err := store.DB().QueryRow(`SELECT direction FROM traders_ratings WHERE code = ?`, code).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if direction("9741") != DirectionDown || direction("1377") != DirectionKeep || direction("6355") != DirectionNew {
		t.Errorf("direction = %s %s %s", direction("9741"), direction("1377"), direction("6355"))
	}
	var rowCount int
	if err := store.DB().QueryRow(`SELECT row_count FROM traders_months WHERE year_month = '200501'`).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 3 {
		t.Errorf("row_count = %d", rowCount)
	}
	months, err := store.ImportedMonths(ctx)
	if err != nil || len(months) != 1 || !months["200501"] {
		t.Errorf("ImportedMonths = %v %v", months, err)
	}

	// 序列表を直した後の計算し直し
	if _, err := store.DB().Exec(`UPDATE traders_ratings SET direction = ''`); err != nil {
		t.Fatal(err)
	}
	n, err := store.RecomputeDirections(ctx)
	if err != nil || n != 3 {
		t.Fatalf("RecomputeDirections = %d %v", n, err)
	}
	if direction("9741") != DirectionDown || direction("6355") != DirectionNew {
		t.Errorf("計算し直した direction = %s %s", direction("9741"), direction("6355"))
	}
}
