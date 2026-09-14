package archive

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// 刈り込みは読む前にロックを取る。ロックを持っている間に別プロセスが日のファイルを
// 差し替えたら、刈り込みは差し替え後の中身を読んで書き戻す（古い中身で上書きしない）。
func TestPruneTakesLockBeforeReading(t *testing.T) {
	ep := ticks()
	ing := newTestIngestor(t, &stubClient{})
	old := "Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n" +
		"2026-09-04,13010,09:00:00.065599,01,4660,1400,000000000012\n" +
		"2026-09-04,13010,12:30:00.000000,01,4662,100,000000000030\n"
	frame, err := CSVToFrame(gzipped(old), ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Archive.Upsert(ep, frame); err != nil {
		t.Fatal(err)
	}
	windows, _ := ParseWindows("09:00-09:10")

	// 別プロセスの Upsert の役: ロックを握ったまま、刈り込みを走らせる
	unlock, err := ing.Archive.lock(ep)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ing.Prune(ep, windows, false)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	select {
	case err := <-done:
		unlock()
		t.Fatalf("ロックを持っている間に刈り込みが終わった: %v", err)
	default:
	}
	// ロックの中で日のファイルを丸ごと差し替える（SplitDay の Upsert と同じ）
	fresh := "Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n" +
		"2026-09-04,13010,09:00:01.000000,01,4700,100,000000000100\n" +
		"2026-09-04,13010,13:00:00.000000,01,4701,100,000000000101\n"
	next, err := CSVToFrame(gzipped(fresh), ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Archive.upsertPart(ep, "2026-09-04", next, nil); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	back, err := ing.Archive.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 1 || *back.Get(0, "TransactionId") != "000000000100" {
		ids := []string{}
		for i := 0; i < back.Height(); i++ {
			ids = append(ids, *back.Get(i, "TransactionId"))
		}
		t.Fatalf("差し替え後の中身で刈っていない（古い読みで上書きした）: %v", ids)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("ディスクがいっぱい") }

// 書き出しに失敗したら .tmp は残らず、元のファイルもそのまま。
func TestWriteFailureLeavesNoTempFile(t *testing.T) {
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	first := frameOf(t, ep, map[string]any{"Date": "2025-01-06", "Code": "1", "Close": "100"})
	if _, err := ing.Archive.Upsert(ep, first); err != nil {
		t.Fatal(err)
	}

	parquetSink = func(*os.File) io.Writer { return brokenWriter{} }
	t.Cleanup(func() { parquetSink = func(handle *os.File) io.Writer { return handle } })

	second := frameOf(t, ep, map[string]any{"Date": "2025-01-07", "Code": "1", "Close": "110"})
	if _, err := ing.Archive.Upsert(ep, second); err == nil {
		t.Fatal("書けないのに成功した")
	}
	entries, err := os.ReadDir(ing.Archive.Directory(ep))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("書きかけのファイルが残っている: %s", e.Name())
		}
	}
	parquetSink = func(handle *os.File) io.Writer { return handle }
	back, err := ing.Archive.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 1 || *back.Get(0, "Close") != "100" {
		t.Fatalf("元のファイルが壊れた: %d 行", back.Height())
	}
}

// 毎営業日行があるはずの端点で「取ったが 0 行」とだけ台帳にある日は欠け。
// 0 行が普通の端点（信用残高）では欠けにしない。後で行が取れたら欠けではなくなる。
func TestGapsCountsEmptyDayOnEveryDayEndpoint(t *testing.T) {
	cal := CalendarEndpoint()
	ing := newTestIngestor(t, &stubClient{})
	f, _ := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "HolDiv": "1"},
		{"Date": "2025-01-07", "HolDiv": "1"},
	}, cal)
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 7, 0, 0, 0, 0, time.UTC)
	now := jstAt(2025, 1, 20, 12, 0)
	margin := MustEndpoint("markets_margin_interest")
	for _, ep := range []Endpoint{bars(), margin} {
		for _, day := range []string{"2025-01-06", "2025-01-07"} {
			if err := ing.Ledger.Record(IngestRecord{
				Endpoint: ep.Path, Target: day, Source: "api", Rows: 0,
				FetchedUTC: jstAt(2025, 1, 7, 17, 0), Digest: "d",
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 1/6 は後で取り直して行が取れた
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: bars().Path, Target: "2025-01-06", Source: "api", Rows: 4000,
		FetchedUTC: jstAt(2025, 1, 8, 17, 0), Digest: "d2",
	}); err != nil {
		t.Fatal(err)
	}

	gaps, err := ing.Gaps(bars(), start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Format(dateLayout) != "2025-01-07" {
		t.Fatalf("日足の 0 行の日 = %v, want [2025-01-07]", gaps)
	}
	gaps, err = ing.Gaps(margin, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 {
		t.Fatalf("週次の端点の 0 行の日を欠けにしている: %v", gaps)
	}
}

// 全件・範囲で取る端点は、最終取得が古ければ Stale に出る。
func TestStale(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	now := jstAt(2026, 9, 15, 8, 0)
	cal := CalendarEndpoint()
	topix := MustEndpoint("indices_bars_daily_topix")
	earnings := MustEndpoint("equities_earnings_calendar")
	investors := MustEndpoint("equities_investor_types")
	record := func(ep Endpoint, at time.Time) {
		t.Helper()
		if err := ing.Ledger.Record(IngestRecord{
			Endpoint: ep.Path, Target: at.Format(dateLayout), Source: "api", Rows: 1, FetchedUTC: at, Digest: "d",
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(cal, now.Add(-10*24*time.Hour))      // 週 1 回の端点。上限は 14 日なのでまだ新しい
	record(topix, now.Add(-8*24*time.Hour))     // 7 日を超えて古い
	record(investors, now.Add(-2*24*time.Hour)) // 新しい
	// earnings は一度も取っていない

	stale, err := ing.Stale([]Endpoint{cal, topix, earnings, investors, bars()}, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Stale{}
	for _, s := range stale {
		got[s.Endpoint.Path] = s
	}
	if len(stale) != 2 {
		t.Fatalf("古い端点 = %+v, want topix と earnings-calendar", stale)
	}
	if s, ok := got[topix.Path]; !ok || s.LastFetched.IsZero() || s.Limit != 7*24*time.Hour {
		t.Errorf("topix = %+v", s)
	}
	if s, ok := got[earnings.Path]; !ok || !s.LastFetched.IsZero() {
		t.Errorf("一度も取っていない端点 = %+v", s)
	}
	// 日数を広げれば topix は古くない
	stale, _ = ing.Stale([]Endpoint{topix}, now, 9)
	if len(stale) != 0 {
		t.Errorf("--stale-days 9 で topix が古い: %+v", stale)
	}
}

func TestStaleLimit(t *testing.T) {
	cases := []struct {
		ep   Endpoint
		days int
		want time.Duration
	}{
		{MustEndpoint("indices_bars_daily_topix"), 0, 7 * 24 * time.Hour},
		{MustEndpoint("indices_bars_daily_topix"), 3, 3 * 24 * time.Hour},
		{CalendarEndpoint(), 0, 14 * 24 * time.Hour}, // 取得間隔 7 日の 2 倍
		{CalendarEndpoint(), 30, 30 * 24 * time.Hour},
	}
	for _, c := range cases {
		if got := StaleLimit(c.ep, c.days); got != c.want {
			t.Errorf("StaleLimit(%s, %d) = %v, want %v", c.ep.Path, c.days, got, c.want)
		}
	}
}

// 窓を後から狭めて取り直しても、既存の窓の外の行は消えない（消すのは prune だけ）。
// 窓の中の行は新しい一括で差し替わる。
func TestNarrowedWindowsKeepRowsOutsideOnReingest(t *testing.T) {
	ep := ticks()
	key := "equities/trades/live/equities_trades_20260904.csv.gz"
	header := "Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n"
	full := header +
		"2026-09-04,13010,08:59:59.000000,01,4660,100,000000000001\n" +
		"2026-09-04,13010,09:00:00.065599,01,4660,1400,000000000012\n" +
		"2026-09-04,13010,12:30:00.000000,01,4662,100,000000000030\n" +
		"2026-09-04,13010,15:30:00.500000,01,4663,100,000000000040\n"
	client := &stubClient{
		bulk:  map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "a"}}},
		files: map[string][]byte{key: gzipped(full)},
	}
	ing := newTestIngestor(t, client)
	t.Setenv(TicksWindowsEnv, "")
	if _, err := ing.Backfill(ep, "", false); err != nil {
		t.Fatal(err)
	}

	// 窓を 09:00-09:10 に狭め、訂正（Price が変わった）で取り直す
	t.Setenv(TicksWindowsEnv, "09:00-09:10")
	client.bulk[ep.Path][0]["LastModified"] = "b"
	client.files[key] = gzipped(strings.Replace(full, "4660,1400", "4999,1400", 1))
	result, err := ing.Backfill(ep, "", false)
	if err != nil || len(result.Failures) != 0 {
		t.Fatalf("%+v %v", result, err)
	}
	back, err := ing.Archive.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 4 {
		t.Fatalf("窓の外の行が消えた: %d 行", back.Height())
	}
	for i := 0; i < back.Height(); i++ {
		if *back.Get(i, "TransactionId") == "000000000012" && *back.Get(i, "Price") != "4999" {
			t.Errorf("窓の中の行が差し替わっていない: Price = %s", *back.Get(i, "Price"))
		}
	}
	// 刈り込みは明示で窓の外を落とせる
	windows, _ := ep.Windows()
	if _, err := ing.Prune(ep, windows, false); err != nil {
		t.Fatal(err)
	}
	back, _ = ing.Archive.Scan(ep)
	if back.Height() != 1 {
		t.Fatalf("prune 後 = %d 行, want 1", back.Height())
	}
}

// 窓があっても、既存ファイルが無ければ普通に書く。窓が無ければ従来どおり丸ごと差し替え。
func TestUpsertKeepingWithoutExistingFile(t *testing.T) {
	ep := ticks()
	ing := newTestIngestor(t, &stubClient{})
	frame, err := CSVToFrame(gzipped(ticksCSV), ep)
	if err != nil {
		t.Fatal(err)
	}
	windows, _ := ParseWindows("09:00-09:10")
	if _, err := ing.upsertWindowed(ep, frame, windows); err != nil {
		t.Fatal(err)
	}
	back, _ := ing.Archive.Scan(ep)
	if back.Height() != 3 {
		t.Fatalf("%d 行", back.Height())
	}
	// 窓無しの差し替えは既存を読まない（1 行だけの塊で日全体が置き換わる）
	one, _ := CSVToFrame(gzipped("Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n"+
		"2026-09-04,13010,14:00:00.000000,01,1,1,000000000099\n"), ep)
	if _, err := ing.upsertWindowed(ep, one, nil); err != nil {
		t.Fatal(err)
	}
	back, _ = ing.Archive.Scan(ep)
	if back.Height() != 1 {
		t.Fatalf("窓無しで丸ごと差し替えになっていない: %d 行", back.Height())
	}
}
