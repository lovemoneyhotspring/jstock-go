package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

func TestLookupEndpoints(t *testing.T) {
	all, err := LookupEndpoints(nil)
	if err != nil || len(all) != len(ActiveEndpoints()) {
		t.Fatalf("空なら全端点: %d %v", len(all), err)
	}
	got, err := LookupEndpoints([]string{"equities_bars_daily", "/markets/calendar", "equities_trades"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Path != "/equities/bars/daily" || got[1].Path != "/markets/calendar" ||
		got[2].Path != "/equities/trades" {
		t.Fatalf("解決 = %+v", got)
	}
	if _, err := LookupEndpoints([]string{"bars", "nope"}); err == nil {
		t.Fatal("未知の端点でエラーにならない")
	}
}

func TestPlanLines(t *testing.T) {
	t.Setenv(TicksEnv, "1")
	cal, topix := CalendarEndpoint(), MustEndpoint("indices_bars_daily_topix")
	jobs := []Job{
		{cal, "2026-09-14", map[string]string{}},
		{topix, "2026-09-14", map[string]string{"to": "2026-09-14", "from": "2026-09-04"}},
		{bars(), "2026-09-11", map[string]string{"date": "2026-09-11"}},
	}
	lines, err := PlanLines(jobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 4 {
		t.Fatalf("行 = %+v", lines)
	}
	// 引数は名前順
	if lines[1].Params != "from=2026-09-04, to=2026-09-14" || lines[0].Params != "" {
		t.Errorf("引数の並び = %q / %q", lines[1].Params, lines[0].Params)
	}
	// API の無い端点は bulk の 1 行
	if lines[3].Endpoint != "/equities/trades" || lines[3].Target != "bulk" {
		t.Errorf("bulk の行 = %+v", lines[3])
	}

	lines, err = PlanLines(jobs, []string{"indices_bars_daily_topix"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Endpoint != topix.Path {
		t.Errorf("--only で絞れていない: %+v", lines)
	}
	lines, _ = PlanLines(jobs, []string{"equities_trades"})
	if len(lines) != 1 || lines[0].Target != "bulk" {
		t.Errorf("--only equities_trades = %+v", lines)
	}
	if _, err := PlanLines(jobs, []string{"nope"}); err == nil {
		t.Error("未知の端点でエラーにならない")
	}
}

// 確認範囲の終わりは JST の今日。09:00 JST より前でも UTC の前日にならない。
func TestCheckRangeUsesJST(t *testing.T) {
	clock.Now = func() time.Time { return time.Date(2026, 9, 14, 23, 30, 0, 0, time.UTC) } // 9/15 08:30 JST
	t.Cleanup(func() { clock.Now = time.Now })

	start, end, err := CheckRange("", 30)
	if err != nil {
		t.Fatal(err)
	}
	if end.Format(dateLayout) != "2026-09-15" || start.Format(dateLayout) != "2026-08-16" {
		t.Fatalf("範囲 = %s 〜 %s", start.Format(dateLayout), end.Format(dateLayout))
	}
	start, end, err = CheckRange("2026-09-10", 0)
	if err != nil {
		t.Fatal(err)
	}
	if end.Location() != clock.Tokyo || end.Format(dateLayout) != "2026-09-10" || !start.Equal(end) {
		t.Fatalf("--date = %v 〜 %v", start, end)
	}
	if _, _, err := CheckRange("2026/09/10", 30); err == nil {
		t.Error("形式違いの --date でエラーにならない")
	}
	if _, _, err := CheckRange("", -1); err == nil {
		t.Error("負の --days でエラーにならない")
	}
}

// JST の範囲を渡しても、営業日の判定（Gaps）は暦日で揃う。
func TestGapsWithJSTRange(t *testing.T) {
	clock.Now = func() time.Time { return jstAt(2025, 1, 9, 8, 30) }
	t.Cleanup(func() { clock.Now = time.Now })
	ep := bars()
	ing := newTestIngestor(t, &stubClient{})
	cal := CalendarEndpoint()
	f, _ := RowsToFrame([]map[string]any{
		{"Date": "2025-01-08", "HolDiv": "1"},
		{"Date": "2025-01-09", "HolDiv": "1"},
	}, cal)
	if _, err := ing.Archive.Upsert(cal, f); err != nil {
		t.Fatal(err)
	}
	start, end, _ := CheckRange("", 1)
	gaps, err := ing.Gaps(ep, start, end, jstAt(2025, 1, 9, 20, 0))
	if err != nil {
		t.Fatal(err)
	}
	// UTC の今日（1/8）で切ると 1/9 を見落とす
	if len(gaps) != 2 || gaps[1].Format(dateLayout) != "2025-01-09" {
		t.Fatalf("欠け = %v", gaps)
	}
}

func TestBackfillAll(t *testing.T) {
	ep := bars()
	indices := MustEndpoint("indices_bars_daily")
	key := "equities_bars_daily_202501.csv.gz"
	client := &stubClient{
		bulk:     map[string][]map[string]any{ep.Path: {{"Key": key, "LastModified": "x"}}},
		files:    map[string][]byte{key: gzipped("Date,Code,Close\n2025-01-06,72030,100\n")},
		failList: indices.Path,
	}
	ing := newTestIngestor(t, client)
	var steps []BackfillStep
	// 一括に無い端点は飛ばし、一覧で失敗した端点があっても後ろは続ける
	result := ing.BackfillAll([]Endpoint{CalendarEndpoint(), indices, ep}, "", false,
		func(s BackfillStep) { steps = append(steps, s) })
	if len(steps) != 3 || !steps[0].Skipped || steps[1].Err == nil || steps[2].Result == nil {
		t.Fatalf("経過 = %+v", steps)
	}
	if len(result.Ingests) != 1 || result.Ingests[0].Endpoint != ep.Path {
		t.Errorf("取り込み = %+v", result.Ingests)
	}
	if len(result.Failures) != 1 || result.Failures[0].Endpoint != indices.Path || result.Failures[0].Target != "bulk:list" {
		t.Errorf("失敗 = %+v", result.Failures)
	}
	// each は nil でもよい
	if r := ing.BackfillAll([]Endpoint{CalendarEndpoint()}, "", false, nil); len(r.Ingests)+len(r.Failures) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestResolveWindows(t *testing.T) {
	ep := ticks()
	t.Setenv(TicksWindowsEnv, "09:00-09:31")
	cases := []struct {
		spec, want string
		fail       bool
	}{
		{"", "09:00-09:31", false},
		{"15:10-15:31,09:00-09:10", "09:00-09:10,15:10-15:31", false},
		{"  ", "09:00-09:31", false},
		{"9-10", "", true},
	}
	for _, c := range cases {
		got, err := ResolveWindows(ep, c.spec)
		if (err != nil) != c.fail {
			t.Errorf("ResolveWindows(%q) err = %v", c.spec, err)
			continue
		}
		if !c.fail && got.String() != c.want {
			t.Errorf("ResolveWindows(%q) = %s, want %s", c.spec, got, c.want)
		}
	}
	// 時刻の列が無い端点は環境変数を見ない（空＝絞らない）
	if got, err := ResolveWindows(bars(), ""); err != nil || len(got) != 0 {
		t.Errorf("bars = %v %v", got, err)
	}
}

func TestJoinDays(t *testing.T) {
	days := func(n int) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = time.Date(2025, 1, 1+i, 0, 0, 0, 0, time.UTC)
		}
		return out
	}
	cases := []struct {
		n    int
		want string
	}{
		{0, ""},
		{2, "2025-01-01, 2025-01-02"},
		{8, "2025-01-01, 2025-01-02, 2025-01-03, 2025-01-04, 2025-01-05, 2025-01-06, 2025-01-07, 2025-01-08"},
		{10, "2025-01-01, 2025-01-02, 2025-01-03, 2025-01-04, 2025-01-05, 2025-01-06, 2025-01-07, 2025-01-08 …（計 10）"},
	}
	for _, c := range cases {
		if got := JoinDays(days(c.n)); got != c.want {
			t.Errorf("JoinDays(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// 保管庫の場所に ' が入っていてもビューを作れる。壊れた端点があっても他は登録し、失敗は返す。
func TestRegisterViews(t *testing.T) {
	root := filepath.Join(t.TempDir(), "it's data")
	arch := NewArchive(root)
	ep := bars()
	f := frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "72030", "Close": "100"},
		map[string]any{"Date": "2025-02-03", "Code": "72030", "Close": "110"})
	if _, err := arch.Upsert(ep, f); err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenDuckDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := RegisterViews(db, arch); err != nil {
		t.Fatalf("登録に失敗: %v", err)
	}
	rows, err := storage.QueryDuckDB(db, "SELECT COUNT(*) AS n FROM equities_bars_daily")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["n"] != int64(2) {
		t.Fatalf("ビューの行数 = %v", rows)
	}

	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "2025-01.parquet"), []byte("not parquet"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = RegisterViews(db, arch)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("壊れた端点のエラーが返らない: %v", err)
	}
	if _, err := storage.QueryDuckDB(db, "SELECT COUNT(*) AS n FROM equities_bars_daily"); err != nil {
		t.Fatalf("壊れた端点のせいで他のビューが使えない: %v", err)
	}
}

func TestQuoteSQL(t *testing.T) {
	if got := quoteLiteral("/data/it's/*.parquet"); got != "'/data/it''s/*.parquet'" {
		t.Errorf("quoteLiteral = %s", got)
	}
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Errorf("quoteIdent = %s", got)
	}
}
