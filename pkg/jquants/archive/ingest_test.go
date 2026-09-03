package archive

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"path/filepath"
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

func TestBulkMonths(t *testing.T) {
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	for _, target := range []string{"bulk:equities_bars_daily_202501.csv.gz", "2025-02-03"} {
		if err := ing.Ledger.Record(IngestRecord{
			Endpoint: ep.Path, Target: target, Source: "x",
			FetchedUTC: jstAt(2025, 2, 4, 12, 0), Digest: "d",
		}); err != nil {
			t.Fatal(err)
		}
	}
	months, err := ing.BulkMonths(ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 1 || !months["2025-01"] {
		t.Fatalf("BulkMonths = %v", months)
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
