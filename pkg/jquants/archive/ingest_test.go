package archive

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// stubClient はネットワークに出ない試験用のクライアント。
type stubClient struct {
	// rows は端点パスごとの応答。
	rows map[string][]map[string]any
	// calls は呼ばれた (パス, 引数) の記録。
	calls []string
	// bulk は /bulk/list の応答、files は key ごとの csv.gz。
	bulk  map[string][]map[string]any
	files map[string][]byte
	// failOn に一致するパスはエラーを返す。
	failOn string
}

func (s *stubClient) GetAll(path string, params map[string]string) ([]map[string]any, error) {
	s.calls = append(s.calls, fmt.Sprintf("%s?%s", path, params["date"]))
	if s.failOn != "" && path == s.failOn {
		return nil, fmt.Errorf("わざと失敗")
	}
	return s.rows[path], nil
}

func (s *stubClient) BulkList(endpoint string) ([]map[string]any, error) {
	return s.bulk[endpoint], nil
}

func (s *stubClient) BulkDownload(key string) ([]byte, error) {
	payload, ok := s.files[key]
	if !ok {
		return nil, fmt.Errorf("そんな鍵は無い: %s", key)
	}
	return payload, nil
}

func newTestIngestor(t *testing.T, client Client) *Ingestor {
	t.Helper()
	root := t.TempDir()
	arch := NewArchive(root)
	ledger, err := OpenLedger(filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	return NewIngestor(client, arch, ledger, "test", nil)
}

// jstAt は JST の時刻を UTC で返す。公開時刻の判定を試すのに使う。
func jstAt(y int, m time.Month, d, hour, minute int) time.Time {
	return time.Date(y, m, d, hour, minute, 0, 0, clock.Tokyo).UTC()
}

func TestIngestStoresAndRecords(t *testing.T) {
	ep := bars()
	client := &stubClient{rows: map[string][]map[string]any{
		ep.Path: {{"Date": "2025-01-06", "Code": "72030", "Close": "100"}},
	}}
	ing := newTestIngestor(t, client)

	day := time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC)
	got, err := ing.IngestDate(ep, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rows != 1 || got.Changed != 1 || got.Source != "api" {
		t.Fatalf("Ingest = %+v", got)
	}
	// Parquet に実体が残っているか（台帳だけ書いて満足しない）
	back, err := ing.Archive.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 1 {
		t.Fatalf("Parquet に保存されていない: %d 行", back.Height())
	}
	last, err := ing.Ledger.Last(ep, "2025-01-06")
	if err != nil || last == nil {
		t.Fatalf("台帳に記録が無い: %v %v", last, err)
	}
	if last.Rows != 1 {
		t.Errorf("台帳の件数 = %d", last.Rows)
	}
}

func TestIngestEmptyResponseIsRecorded(t *testing.T) {
	// 提出の無い日（EDINET 等）は 0 行。取ったことは残す（欠けと区別するため）
	ep := MustEndpoint("edinet_major_shareholders")
	ing := newTestIngestor(t, &stubClient{})
	got, err := ing.IngestDate(ep, time.Date(2025, 1, 6, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Rows != 0 {
		t.Errorf("Rows = %d", got.Rows)
	}
	if last, _ := ing.Ledger.Last(ep, "2025-01-06"); last == nil {
		t.Error("空でも台帳には残すべき")
	}
}

func TestPlanWaitsForAvailableAt(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	ep := bars() // 16:30 JST 公開

	// カレンダーが無いので平日で代用される。1/6 は月曜
	before := jstAt(2025, 1, 6, 15, 0)
	jobs, err := ing.Plan(before, -1)
	if err != nil {
		t.Fatal(err)
	}
	if hasJob(jobs, ep.Path, "2025-01-06") {
		t.Error("公開時刻の前に当日ぶんを取ろうとしている")
	}
	after := jstAt(2025, 1, 6, 17, 0)
	jobs, err = ing.Plan(after, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasJob(jobs, ep.Path, "2025-01-06") {
		t.Error("公開時刻を過ぎたら当日ぶんが対象のはず")
	}
}

func TestPlanSkipsRecentlyFetched(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	ep := bars()
	now := jstAt(2025, 1, 6, 17, 0)
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "2025-01-06", Source: "api",
		FetchedUTC: now.Add(-1 * time.Hour), Rows: 1, Changed: 1, Digest: "d",
	}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := ing.Plan(now, -1)
	if hasJob(jobs, ep.Path, "2025-01-06") {
		// min_interval_hours = 20。1 時間前に取ったばかりなら叩き直さない
		t.Error("直前に取った日を取り直そうとしている")
	}
}

func TestPlanRangeEndpointUsesFromTo(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	ep := MustEndpoint("indices_bars_daily_topix") // RangeDays = 10
	jobs, _ := ing.Plan(jstAt(2025, 1, 20, 17, 0), -1)
	for _, job := range jobs {
		if job.Endpoint.Path != ep.Path {
			continue
		}
		if job.Params["from"] != "2025-01-10" || job.Params["to"] != "2025-01-20" {
			t.Fatalf("範囲が違う: %v", job.Params)
		}
		return
	}
	t.Fatal("範囲の端点が計画に出てこない")
}

func TestPlanBackfillSkipsFetchedDays(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	ep := bars()
	now := jstAt(2025, 1, 20, 17, 0)
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "2025-01-15", Source: "api",
		FetchedUTC: now.Add(-240 * time.Hour), Rows: 1, Changed: 1, Digest: "d",
	}); err != nil {
		t.Fatal(err)
	}
	jobs, err := ing.Plan(now, 10) // --days 10 の遡り
	if err != nil {
		t.Fatal(err)
	}
	if hasJob(jobs, ep.Path, "2025-01-15") {
		t.Error("遡りは「一度も取っていない日」だけのはず")
	}
	if !hasJob(jobs, ep.Path, "2025-01-14") {
		t.Error("未取得の日は遡りの対象のはず")
	}
}

func TestSyncCollectsFailures(t *testing.T) {
	ep := bars()
	client := &stubClient{
		rows:   map[string][]map[string]any{},
		failOn: ep.Path,
	}
	ing := newTestIngestor(t, client)
	result, err := ing.Sync(jstAt(2025, 1, 6, 17, 0), -1, []string{ep.Name()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) == 0 {
		t.Fatal("失敗が集められていない")
	}
	if result.Failures[0].Endpoint != ep.Path {
		t.Errorf("失敗の端点 = %s", result.Failures[0].Endpoint)
	}
}

func TestSyncOnlyFiltersEndpoints(t *testing.T) {
	ep := bars()
	client := &stubClient{rows: map[string][]map[string]any{
		ep.Path: {{"Date": "2025-01-06", "Code": "72030", "Close": "100"}},
	}}
	ing := newTestIngestor(t, client)
	result, err := ing.Sync(jstAt(2025, 1, 6, 17, 0), -1, []string{ep.Name()})
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range result.Ingests {
		if got.Endpoint != ep.Path {
			t.Errorf("--only で絞ったのに %s を取っている", got.Endpoint)
		}
	}
	if len(result.Ingests) == 0 {
		t.Error("何も取っていない")
	}
}

func TestSyncTakesCalendarFirst(t *testing.T) {
	cal := CalendarEndpoint()
	client := &stubClient{rows: map[string][]map[string]any{
		cal.Path: {{"Date": "2025-01-06", "HolDiv": "1"}},
	}}
	ing := newTestIngestor(t, client)
	result, err := ing.Sync(jstAt(2025, 1, 6, 17, 0), -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ingests) == 0 || result.Ingests[0].Endpoint != cal.Path {
		t.Fatalf("取引カレンダーを最初に取っていない: %+v", result.Ingests)
	}
}

func TestTradingDaysFromCalendar(t *testing.T) {
	cal := CalendarEndpoint()
	ing := newTestIngestor(t, &stubClient{})
	f, err := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "HolDiv": "1"},
		{"Date": "2025-01-07", "HolDiv": "0"}, // 非営業日
		{"Date": "2025-01-08", "HolDiv": "2"}, // 半日は営業日扱い
	}, cal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	days, err := ing.TradingDays(
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("営業日 = %v", days)
	}
	if days[0].Format("2006-01-02") != "2025-01-06" || days[1].Format("2006-01-02") != "2025-01-08" {
		t.Errorf("営業日の中身が違う: %v", days)
	}
}

func TestGaps(t *testing.T) {
	cal := CalendarEndpoint()
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	f, _ := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "HolDiv": "1"},
		{"Date": "2025-01-07", "HolDiv": "1"},
		{"Date": "2025-01-08", "HolDiv": "1"},
	}, cal)
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	// 1/6 はデータあり、1/7 は「取ったが 0 件」、1/8 は欠け
	bars6, _ := RowsToFrame([]map[string]any{{"Date": "2025-01-06", "Code": "1", "C": "1"}}, ep)
	if _, err := ing.Archive.Upsert(ep, bars6); err != nil {
		t.Fatal(err)
	}
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "2025-01-07", Source: "api",
		FetchedUTC: jstAt(2025, 1, 7, 18, 0), Digest: "d",
	}); err != nil {
		t.Fatal(err)
	}
	gaps, err := ing.Gaps(ep,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		jstAt(2025, 2, 1, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 1 || gaps[0].Format("2006-01-02") != "2025-01-08" {
		t.Fatalf("欠け = %v, want [2025-01-08]", gaps)
	}
}

func TestGapsIgnoresBulkCoveredMonths(t *testing.T) {
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "bulk:equities_bars_daily_202501.csv.gz", Source: "bulk",
		FetchedUTC: jstAt(2025, 2, 1, 12, 0), Digest: "stamp",
	}); err != nil {
		t.Fatal(err)
	}
	gaps, err := ing.Gaps(ep,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		jstAt(2025, 2, 1, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 {
		t.Fatalf("一括で埋めた月を欠けとみなしている: %v", gaps)
	}
}

func TestGapsSkipsNonDateModes(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	gaps, err := ing.Gaps(CalendarEndpoint(),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC), jstAt(2025, 2, 1, 12, 0))
	if err != nil || gaps != nil {
		t.Errorf("日付モード以外に欠けの概念は無い: %v %v", gaps, err)
	}
}

func TestBackfill(t *testing.T) {
	ep := bars()
	key := "equities_bars_daily_202501.csv.gz"
	client := &stubClient{
		bulk:  map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "2025-02-01T00:00:00Z"}}},
		files: map[string][]byte{key: gzipped("Date,Code,Close\n2025-01-06,72030,100\n")},
	}
	ing := newTestIngestor(t, client)
	result, err := ing.Backfill(ep, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Rows != 1 {
		t.Fatalf("一括取り込みの結果 = %+v", result.Ingests)
	}
	back, _ := ing.Archive.Scan(ep)
	if back.Height() != 1 {
		t.Fatalf("Parquet に落ちていない: %d 行", back.Height())
	}
	// 生の csv.gz を保険として残す
	if _, err := filepath.Glob(filepath.Join(ing.Archive.RawDir(ep), key)); err != nil {
		t.Fatal(err)
	}

	// 2 度目は LastModified が同じなので飛ばす
	again, err := ing.Backfill(ep, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Ingests) != 0 {
		t.Errorf("同じ LastModified を取り直している: %+v", again.Ingests)
	}
}

func TestBackfillSince(t *testing.T) {
	ep := bars()
	old, recent := "equities_bars_daily_202401.csv.gz", "equities_bars_daily_202501.csv.gz"
	client := &stubClient{
		bulk: map[string][]map[string]any{ep.Path: {
			{"Key": old, "LastModified": "a"},
			{"Key": recent, "LastModified": "b"},
		}},
		files: map[string][]byte{
			old:    gzipped("Date,Code,Close\n2024-01-05,1,1\n"),
			recent: gzipped("Date,Code,Close\n2025-01-06,1,2\n"),
		},
	}
	ing := newTestIngestor(t, client)
	result, err := ing.Backfill(ep, "2025-01", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Target != "bulk:"+recent {
		t.Fatalf("--since で絞れていない: %+v", result.Ingests)
	}
}

func TestBackfillRejectsNonBulkEndpoint(t *testing.T) {
	ing := newTestIngestor(t, &stubClient{})
	if _, err := ing.Backfill(CalendarEndpoint(), "", false); err == nil {
		t.Error("一括に無い端点はエラーにすべき")
	}
}

func TestBulkCoverage(t *testing.T) {
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	// J-Quants の一括は過去が月次（historical/）、当月が日次（live/）で配られる。
	// 日次ファイルを月として数えると、その月の他の日が欠けていても見逃してしまう
	targets := []string{
		"bulk:equities/bars/daily/historical/2025/equities_bars_daily_202501.csv.gz",
		"bulk:equities/bars/daily/live/equities_bars_daily_20250203.csv.gz",
		"2025-02-04", // 一括ではない普通の取り込み記録
	}
	for _, target := range targets {
		if err := ing.Ledger.Record(IngestRecord{
			Endpoint: ep.Path, Target: target, Source: "x",
			FetchedUTC: jstAt(2025, 2, 5, 12, 0), Digest: "d",
		}); err != nil {
			t.Fatal(err)
		}
	}
	covered, err := ing.BulkCoverage(ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(covered) != 2 || !covered["2025-01"] || !covered["2025-02-03"] {
		t.Fatalf("BulkCoverage = %v", covered)
	}
	// 月次ファイルの月はその月の全日を覆う
	if !covers(covered, time.Date(2025, 1, 20, 0, 0, 0, 0, time.UTC)) {
		t.Error("月次ファイルの月内の日が覆われていない")
	}
	// 日次ファイルはその日だけ。同じ月の他の日は覆わない
	if !covers(covered, time.Date(2025, 2, 3, 0, 0, 0, 0, time.UTC)) {
		t.Error("日次ファイルの当日が覆われていない")
	}
	if covers(covered, time.Date(2025, 2, 10, 0, 0, 0, 0, time.UTC)) {
		t.Error("日次ファイルが月全体を覆ってしまっている（当月の欠けを見逃す）")
	}
}

func TestCoverageIn(t *testing.T) {
	cases := map[string]string{
		"equities/bars/minute/historical/2026/equities_bars_minute_202608.csv.gz": "2026-08",
		"equities/bars/minute/live/equities_bars_minute_20260904.csv.gz":          "2026-09-04",
		"equities_bars_daily_202501.csv.gz":                                       "2025-01",
		"なにか別のファイル.csv.gz":                                                        "",
	}
	for key, want := range cases {
		if got := coverageIn(key); got != want {
			t.Errorf("coverageIn(%q) = %q, want %q", key, got, want)
		}
	}
}

func hasJob(jobs []Job, path, target string) bool {
	for _, job := range jobs {
		if job.Endpoint.Path == path && job.Target == target {
			return true
		}
	}
	return false
}

func gzipped(text string) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	return buf.Bytes()
}

// ticksCSV は 2026-09-04 の一括ティックと同じ形の 1 日ぶん。
const ticksCSV = "Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n" +
	"2026-09-04,13010,09:00:00.065599,01,4660,1400,000000000012\n" +
	"2026-09-04,13010,09:00:00.083619,01,4660,600,000000000030\n" +
	"2026-09-04,72030,09:00:00.100000,01,2500.5,100,000000000031\n"

func TestSyncTakesBulkOnlyEndpointFromDailyFiles(t *testing.T) {
	t.Setenv(TicksEnv, "1")
	ep := ticks()
	key := "equities/trades/live/equities_trades_20260904.csv.gz"
	client := &stubClient{
		bulk:  map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "2026-09-04T07:23:33+00:00"}}},
		files: map[string][]byte{key: gzipped(ticksCSV)},
	}
	ing := newTestIngestor(t, client)
	now := jstAt(2026, 9, 4, 17, 0)

	// API の date= では叩かない
	jobs, err := ing.Plan(now, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if job.Endpoint.Path == ep.Path {
			t.Fatalf("API の無い端点に date= の仕事が立っている: %+v", job)
		}
	}

	result, err := ing.Sync(now, -1, []string{"equities_trades"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 {
		t.Fatalf("失敗: %+v", result.Failures)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Rows != 3 || result.Ingests[0].Source != "bulk" {
		t.Fatalf("一括の日次ファイルを取っていない: %+v", result.Ingests)
	}
	for _, c := range client.calls {
		if strings.HasPrefix(c, ep.Path) {
			t.Errorf("API を叩いている: %s", c)
		}
	}
	// 1 日 1 ファイルの Parquet に、鍵順で落ちている
	if _, err := os.Stat(ing.Archive.PathFor(ep, "2026-09-04")); err != nil {
		t.Fatalf("日分割の Parquet が無い: %v", err)
	}
	back, err := ing.Archive.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 3 {
		t.Fatalf("行数 = %d", back.Height())
	}
	if got := *back.Get(0, "TransactionId"); got != "000000000012" {
		t.Errorf("TransactionId の先頭ゼロが落ちた: %s", got)
	}
	// _raw は残さない（月 1GB）
	if entries, _ := os.ReadDir(ing.Archive.RawDir(ep)); len(entries) != 0 {
		t.Errorf("ティックの _raw が残っている: %v", entries)
	}
	// 欠けの判定は一括の日次ファイルを「取った日」と見る
	gaps, err := ing.Gaps(ep, time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 {
		t.Errorf("取った日を欠けとみなしている: %v", gaps)
	}

	// 2 度目は LastModified が同じなので何もしない
	again, err := ing.Sync(now, -1, []string{"equities_trades"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Ingests) != 0 {
		t.Errorf("同じファイルを取り直している: %+v", again.Ingests)
	}
}

func TestSyncBulkSkipsMonthsBeforeLookback(t *testing.T) {
	ep := ticks()
	old, recent := "equities/trades/historical/2026/equities_trades_202607.csv.gz", "equities/trades/live/equities_trades_20260904.csv.gz"
	client := &stubClient{
		bulk: map[string][]map[string]any{ep.Path: {
			{"Key": old, "LastModified": "a"},
			{"Key": recent, "LastModified": "b"},
		}},
		files: map[string][]byte{
			old:    gzipped("Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n2026-07-01,1,09:00:00.000000,01,1,1,1\n"),
			recent: gzipped(ticksCSV),
		},
	}
	ing := newTestIngestor(t, client)
	result, err := ing.SyncBulk(ep, jstAt(2026, 9, 4, 17, 0), -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Target != "bulk:"+recent {
		t.Fatalf("遡りの範囲より前の月次ファイルまで取っている: %+v", result.Ingests)
	}
}

func TestIngestTrimsToWindows(t *testing.T) {
	t.Setenv(TicksWindowsEnv, "09:00-09:00") // 不正（開始 = 終了）はエラーで止まる
	ep := ticks()
	key := "equities/trades/live/equities_trades_20260904.csv.gz"
	client := &stubClient{
		bulk:  map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "x"}}},
		files: map[string][]byte{key: gzipped(ticksCSV)},
	}
	ing := newTestIngestor(t, client)
	result, err := ing.Backfill(ep, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 1 {
		t.Fatalf("読めない時間帯で取り込みが進んだ: %+v", result)
	}

	// 09:00:00.08 以降だけ残す窓は無いので、秒単位の窓で 2 行目・3 行目を落とす例にする
	t.Setenv(TicksWindowsEnv, "15:10-15:31")
	result, err = ing.Backfill(ep, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Failures) != 0 || len(result.Ingests) != 1 {
		t.Fatalf("%+v", result)
	}
	// 3 行とも 09:00 なので全部落ち、Parquet は書かれない（0 行の Upsert は何もしない）
	if result.Ingests[0].Rows != 0 {
		t.Errorf("窓の外の行が残っている: %+v", result.Ingests[0])
	}

	t.Setenv(TicksWindowsEnv, "09:00-09:10")
	// 台帳の LastModified が同じなので、取り直させるために別の stamp にする
	client.bulk[ep.Path][0]["LastModified"] = "y"
	result, err = ing.Backfill(ep, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Rows != 3 {
		t.Errorf("窓の中の行が落ちた: %+v", result.Ingests)
	}
}

func TestPruneKeepsOnlyWindows(t *testing.T) {
	ep := ticks()
	ing := newTestIngestor(t, &stubClient{})
	csv := "Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n" +
		"2026-09-04,13010,08:59:59.000000,01,4660,100,000000000001\n" +
		"2026-09-04,13010,09:00:00.065599,01,4660,1400,000000000012\n" +
		"2026-09-04,13010,09:10:00.000000,01,4661,100,000000000020\n" +
		"2026-09-04,13010,12:30:00.000000,01,4662,100,000000000030\n" +
		"2026-09-04,13010,15:30:00.500000,01,4663,100,000000000040\n"
	frame, err := CSVToFrame(gzipped(csv), ep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ing.Archive.Upsert(ep, frame); err != nil {
		t.Fatal(err)
	}
	windows, _ := ParseWindows("09:00-09:10,15:10-15:31")

	// 数えるだけ
	dry, err := ing.Prune(ep, windows, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry) != 1 || dry[0].Before != 5 || dry[0].After != 2 || dry[0].Written {
		t.Fatalf("dry-run = %+v", dry)
	}
	back, _ := ing.Archive.Scan(ep)
	if back.Height() != 5 {
		t.Fatalf("dry-run が書き換えた: %d 行", back.Height())
	}

	got, err := ing.Prune(ep, windows, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Written || got[0].After != 2 {
		t.Fatalf("prune = %+v", got)
	}
	back, _ = ing.Archive.Scan(ep)
	if back.Height() != 2 {
		t.Fatalf("残った行数 = %d", back.Height())
	}
	if *back.Get(0, "TransactionId") != "000000000012" || *back.Get(1, "TransactionId") != "000000000040" {
		t.Errorf("残った行が違う: %s %s", *back.Get(0, "TransactionId"), *back.Get(1, "TransactionId"))
	}
	// 型付きの列は型のまま書き戻されている（Price は数値として読める）
	if *back.Get(1, "Price") != "4663" {
		t.Errorf("Price = %s", *back.Get(1, "Price"))
	}
	// 台帳に prune の記録
	last, err := ing.Ledger.Last(ep, "2026-09-04")
	if err != nil || last == nil || last.Source != "prune" || last.Changed != 3 {
		t.Errorf("台帳の記録 = %+v %v", last, err)
	}
	// 2 度目は落とす行が無いので触らない
	again, _ := ing.Prune(ep, windows, false)
	if again[0].Written {
		t.Error("変化が無いのに書き換えている")
	}
	// 月分割の端点は対象外
	if _, err := ing.Prune(bars(), windows, true); err == nil {
		t.Error("月分割の端点を刈ろうとしている")
	}
}

func TestGapsSkipsBeforeFirstKnownDay(t *testing.T) {
	// EDINET やアドオンのように途中から始まる端点は、最初に持っている日より前を欠けと数えない
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "bulk:equities_bars_daily_202502.csv.gz", Source: "bulk",
		FetchedUTC: jstAt(2025, 3, 1, 12, 0), Digest: "stamp",
	}); err != nil {
		t.Fatal(err)
	}
	gaps, err := ing.Gaps(ep,
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 3, 5, 0, 0, 0, 0, time.UTC),
		jstAt(2025, 3, 6, 12, 0))
	if err != nil {
		t.Fatal(err)
	}
	// 1 月は始まる前なので数えず、3 月（一括に無い）だけ欠け
	for _, g := range gaps {
		if g.Month() != time.March {
			t.Fatalf("始まる前の日を欠けと数えている: %v", gaps)
		}
	}
	if len(gaps) == 0 {
		t.Fatal("一括の後の欠けを見落としている")
	}
}

func TestFirstKnown(t *testing.T) {
	cases := []struct {
		dates   []string
		targets []string
		want    string
	}{
		{nil, nil, ""},
		{[]string{"2025-01-08"}, []string{"2025-01-07"}, "2025-01-07"},
		{nil, []string{"bulk:equities_bars_daily_202501.csv.gz"}, "2025-01-01"},
		{nil, []string{"bulk:equities_trades_20250107.csv.gz", "2025-01-09"}, "2025-01-07"},
		{[]string{"2024-12-30"}, []string{"bulk:x_202501.csv.gz"}, "2024-12-30"},
	}
	for _, c := range cases {
		var dates []time.Time
		for _, d := range c.dates {
			parsed, _ := time.Parse(dateLayout, d)
			dates = append(dates, parsed)
		}
		if got := firstKnown(dates, c.targets); got != c.want {
			t.Errorf("firstKnown(%v, %v) = %q, want %q", c.dates, c.targets, got, c.want)
		}
	}
}

func TestRepairFillsGapsAndRecordsEmptyDays(t *testing.T) {
	cal := CalendarEndpoint()
	ep := bars()
	client := &stubClient{rows: map[string][]map[string]any{
		ep.Path: {{"Date": "2025-01-08", "Code": "1", "C": "1"}},
	}}
	ing := newTestIngestor(t, client)
	f, _ := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "HolDiv": "1"},
		{"Date": "2025-01-07", "HolDiv": "1"},
		{"Date": "2025-01-08", "HolDiv": "1"},
	}, cal)
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	bars6, _ := RowsToFrame([]map[string]any{{"Date": "2025-01-06", "Code": "1", "C": "1"}}, ep)
	if _, err := ing.Archive.Upsert(ep, bars6); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 8, 0, 0, 0, 0, time.UTC)
	now := jstAt(2025, 1, 9, 12, 0)

	plans, err := ing.PlanRepair([]Endpoint{cal, ep}, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].Days) != 2 {
		t.Fatalf("計画 = %+v, want bars の 1/7 と 1/8", plans)
	}

	result, err := ing.Repair([]Endpoint{cal, ep}, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	// スタブは date= を見ないので両日とも同じ 1/8 の行が返るが、
	// 1/7 の取り込みも台帳に残る（週次で 0 行の日と同じ扱い）
	if len(result.Ingests) != 2 || len(result.Failures) != 0 {
		t.Fatalf("結果 = %+v", result)
	}
	if len(result.Remaining) != 0 {
		t.Fatalf("埋めた後にまだ欠けている: %+v", result.Remaining)
	}
	if len(client.calls) != 2 || client.calls[0] != ep.Path+"?2025-01-07" || client.calls[1] != ep.Path+"?2025-01-08" {
		t.Fatalf("呼び出し = %v", client.calls)
	}
}

func TestRepairBulkOnlyUsesDailyFiles(t *testing.T) {
	t.Setenv("JQUANTS_TICKS", "1")
	cal := CalendarEndpoint()
	ep := ticks()
	key := "equities/trades/live/equities_trades_20250107.csv.gz"
	client := &stubClient{
		bulk:  map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "2025-01-08T00:00:00Z"}}},
		files: map[string][]byte{key: gzipped("Date,Code,Time,SessionDistinction,Price,TradingVolume,TransactionId\n2025-01-07,1,09:00:00.000000,01,1,1,1\n")},
	}
	ing := newTestIngestor(t, client)
	f, _ := RowsToFrame([]map[string]any{
		{"Date": "2025-01-06", "HolDiv": "1"},
		{"Date": "2025-01-07", "HolDiv": "1"},
	}, cal)
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	// 1/6 は一括で取り込み済み、1/7 が欠け
	if err := ing.Ledger.Record(IngestRecord{
		Endpoint: ep.Path, Target: "bulk:equities_trades_20250106.csv.gz", Source: "bulk",
		FetchedUTC: jstAt(2025, 1, 7, 12, 0), Digest: "stamp",
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2025, 1, 7, 0, 0, 0, 0, time.UTC)
	now := jstAt(2025, 1, 8, 12, 0)
	result, err := ing.Repair([]Endpoint{ep}, start, end, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plans) != 1 || len(result.Plans[0].Days) != 1 {
		t.Fatalf("計画 = %+v", result.Plans)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Target != "bulk:"+key {
		t.Fatalf("取り込み = %+v, 失敗 = %+v", result.Ingests, result.Failures)
	}
	if len(result.Remaining) != 0 || len(client.calls) != 0 {
		t.Fatalf("残り = %+v, API 呼び出し = %v", result.Remaining, client.calls)
	}
}
