package archive

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// bars は試験でよく使う端点（日次の足）。
func bars() Endpoint { return MustEndpoint("equities_bars_daily") }

func frameOf(t *testing.T, ep Endpoint, rows ...map[string]any) *Frame {
	t.Helper()
	f, err := RowsToFrame(rows, ep)
	if err != nil {
		t.Fatalf("RowsToFrame: %v", err)
	}
	return f
}

func TestEndpointName(t *testing.T) {
	cases := map[string]string{
		"/equities/bars/daily":                "equities_bars_daily",
		"/fins/earnings-date":                 "fins_earnings_date",
		"/markets/short-ratio":                "markets_short_ratio",
		"/derivatives/bars/daily/options/225": "derivatives_bars_daily_options_225",
	}
	for path, want := range cases {
		ep := Endpoint{Path: path}
		if got := ep.Name(); got != want {
			t.Errorf("%s: Name = %q, want %q", path, got, want)
		}
	}
}

func TestStandardEndpointsCoverage(t *testing.T) {
	// Python の ENDPOINTS と同じ 17 個。減っていると蓄積の穴になる
	if len(StandardEndpoints) != 17 {
		t.Fatalf("端点の数 = %d, want 17", len(StandardEndpoints))
	}
	seen := map[string]bool{}
	for _, ep := range StandardEndpoints {
		if seen[ep.Path] {
			t.Errorf("端点が重複: %s", ep.Path)
		}
		seen[ep.Path] = true
		if len(ep.Key) == 0 || ep.DateColumn == "" {
			t.Errorf("%s: 鍵か日付列が空", ep.Path)
		}
		if ep.Mode == "" || ep.DateParam == "" {
			t.Errorf("%s: Mode / DateParam が未設定", ep.Path)
		}
	}
	if _, err := LookupEndpoint("equities_bars_daily"); err != nil {
		t.Errorf("名前で引けない: %v", err)
	}
	if _, err := LookupEndpoint("/equities/bars/daily"); err != nil {
		t.Errorf("パスで引けない: %v", err)
	}
	if _, err := LookupEndpoint("nope"); err == nil {
		t.Error("未知の端点でエラーにならない")
	}
}

func TestRowsToFrameStringifies(t *testing.T) {
	ep := bars()
	f := frameOf(t, ep, map[string]any{
		"Date": "2025-01-06", "Code": "72030", "Close": 1234.5, "Vol": 100, "Ok": true, "Missing": nil,
	})
	if f.Height() != 1 {
		t.Fatalf("行数 = %d", f.Height())
	}
	want := map[string]string{"Date": "2025-01-06", "Code": "72030", "Close": "1234.5", "Vol": "100", "Ok": "true"}
	for k, v := range want {
		got := f.Get(0, k)
		if got == nil || *got != v {
			t.Errorf("%s = %v, want %q", k, got, v)
		}
	}
	if f.Get(0, "Missing") != nil {
		t.Error("null は nil のまま残るべき")
	}
}

func TestRowsToFrameNormalizesDates(t *testing.T) {
	ep := MustEndpoint("fins_summary")
	f := frameOf(t, ep, map[string]any{
		"DiscDate": "2025/01/06", "DiscTime": "15:00", "Code": "72030", "DiscNo": "1",
		"CurPerSt": "", "CurFYEn": "2025-03-31",
	})
	if got := f.Get(0, "DiscDate"); got == nil || *got != "2025-01-06" {
		t.Errorf("スラッシュ区切りが正規化されていない: %v", got)
	}
	if f.Get(0, "CurPerSt") != nil {
		t.Error("空の日付は NULL であるべき")
	}
	if got := f.Get(0, "CurFYEn"); got == nil || *got != "2025-03-31" {
		t.Errorf("追加の日付列が壊れた: %v", got)
	}
}

func TestRowsToFrameRequiresDateColumn(t *testing.T) {
	if _, err := RowsToFrame([]map[string]any{{"Code": "72030"}}, bars()); err == nil {
		t.Error("日付列が無ければエラーにすべき")
	}
}

func TestCSVToFrame(t *testing.T) {
	csv := "Date,Code,Close\n2025-01-06,72030,1234\n2025-01-07,72030,\n"
	f, err := CSVToFrame([]byte(csv), bars())
	if err != nil {
		t.Fatalf("CSVToFrame: %v", err)
	}
	if f.Height() != 2 {
		t.Fatalf("行数 = %d", f.Height())
	}
	if f.Get(1, "Close") != nil {
		t.Error("空欄は NULL であるべき")
	}
	if got := f.Get(0, "Date"); got == nil || *got != "2025-01-06" {
		t.Errorf("Date = %v", got)
	}
}

func TestUpsertRoundTrip(t *testing.T) {
	root := t.TempDir()
	a := NewArchive(root)
	ep := bars()

	changed, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "72030", "Close": "100"},
		map[string]any{"Date": "2025-01-07", "Code": "72030", "Close": "110"},
	))
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if changed != 2 {
		t.Errorf("初回の変化 = %d, want 2", changed)
	}
	if got := a.Months(ep); len(got) != 1 || got[0] != "2025-01" {
		t.Fatalf("Months = %v", got)
	}
	if _, err := os.Stat(a.PathFor(ep, "2025-01")); err != nil {
		t.Fatalf("Parquet が無い: %v", err)
	}

	back, err := a.Scan(ep)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if back.Height() != 2 {
		t.Fatalf("読み戻した行数 = %d", back.Height())
	}
	if got := back.Get(0, "Close"); got == nil || *got != "100" {
		t.Errorf("Close = %v", got)
	}
	if got := back.Get(0, "Date"); got == nil || *got != "2025-01-06" {
		t.Errorf("Date の往復に失敗: %v", got)
	}
}

func TestUpsertLastWins(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	if _, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "72030", "Close": "100"})); err != nil {
		t.Fatal(err)
	}
	// 同じ鍵で取り直す（速報 → 確報）
	changed, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "72030", "Close": "101"}))
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Errorf("変化 = %d, want 1", changed)
	}
	back, _ := a.Scan(ep)
	if back.Height() != 1 {
		t.Fatalf("後勝ちで 1 行になるはず: %d", back.Height())
	}
	if got := back.Get(0, "Close"); got == nil || *got != "101" {
		t.Errorf("Close = %v, want 101", got)
	}

	// 内容が同じなら「変化」は 0（冪等）
	changed, err = a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "72030", "Close": "101"}))
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 {
		t.Errorf("同じ内容の取り直しで変化 = %d, want 0", changed)
	}
}

func TestUpsertSplitsMonths(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	if _, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-31", "Code": "1", "C": "1"},
		map[string]any{"Date": "2025-02-03", "Code": "1", "C": "2"},
	)); err != nil {
		t.Fatal(err)
	}
	months := a.Months(ep)
	if len(months) != 2 || months[0] != "2025-01" || months[1] != "2025-02" {
		t.Fatalf("月ごとに分かれていない: %v", months)
	}
}

func TestUpsertNewColumn(t *testing.T) {
	// 仕様変更で列が増えても古い行と一緒に読める（列は和集合）
	a := NewArchive(t.TempDir())
	ep := bars()
	if _, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "1", "C": "1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-07", "Code": "1", "C": "2", "New": "x"})); err != nil {
		t.Fatal(err)
	}
	back, err := a.Scan(ep)
	if err != nil {
		t.Fatal(err)
	}
	if back.Height() != 2 || !back.HasColumn("New") {
		t.Fatalf("列の和集合になっていない: %v / %d 行", back.Columns, back.Height())
	}
}

func TestUpsertRequiresKeyColumns(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	f := &Frame{Columns: []string{"Date"}, Rows: []Row{{strptr("2025-01-06")}}}
	if _, err := a.Upsert(ep, f); err == nil {
		t.Error("鍵の列が無ければエラーにすべき")
	}
}

func TestReadRange(t *testing.T) {
	a := NewArchive(t.TempDir())
	ep := bars()
	if _, err := a.Upsert(ep, frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "1", "C": "1"},
		map[string]any{"Date": "2025-02-03", "Code": "1", "C": "2"},
		map[string]any{"Date": "2025-03-03", "Code": "1", "C": "3"},
	)); err != nil {
		t.Fatal(err)
	}
	day := func(s string) time.Time {
		d, _ := time.Parse("2006-01-02", s)
		return d
	}
	got, err := a.Read(ep, day("2025-02-01"), day("2025-02-28"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Height() != 1 {
		t.Fatalf("期間で絞れていない: %d 行", got.Height())
	}
	dates, err := a.Dates(ep)
	if err != nil {
		t.Fatal(err)
	}
	if len(dates) != 3 || dates[0].Format("2006-01-02") != "2025-01-06" {
		t.Fatalf("Dates = %v", dates)
	}
}

func TestDigestOfIsOrderInsensitive(t *testing.T) {
	ep := bars()
	a := frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "1", "C": "1"},
		map[string]any{"Date": "2025-01-07", "Code": "1", "C": "2"})
	b := frameOf(t, ep,
		map[string]any{"Date": "2025-01-07", "Code": "1", "C": "2"},
		map[string]any{"Date": "2025-01-06", "Code": "1", "C": "1"})
	if DigestOf(a) != DigestOf(b) {
		t.Error("行順で digest が変わってはいけない")
	}
	c := frameOf(t, ep, map[string]any{"Date": "2025-01-06", "Code": "1", "C": "9"})
	if DigestOf(a) == DigestOf(c) {
		t.Error("内容が違えば digest も違うべき")
	}
	if DigestOf(&Frame{}) == "" {
		t.Error("空の digest が空文字")
	}
}

func TestTyped(t *testing.T) {
	ep := bars()
	f := frameOf(t, ep,
		map[string]any{"Date": "2025-01-06", "Code": "07203", "Close": "1234.5", "Name": "トヨタ"},
		map[string]any{"Date": "2025-01-07", "Code": "07203", "Close": "1240", "Name": "トヨタ"})
	typed := Typed(f)
	if !typed.Numeric["Close"] {
		t.Error("Close は数値列のはず")
	}
	if typed.Numeric["Code"] {
		t.Error("Code は数字だけでも数値にしない")
	}
	if typed.Numeric["Name"] {
		t.Error("文字列の列を数値にしてはいけない")
	}
	if v, ok := typed.Float(0, "Close"); !ok || v != 1234.5 {
		t.Errorf("Float = %v, %v", v, ok)
	}
}

func TestAsOf(t *testing.T) {
	ep := MustEndpoint("fins_summary")
	f := frameOf(t, ep,
		map[string]any{"DiscDate": "2025-01-06", "DiscTime": "15:00", "Code": "A", "DiscNo": "1", "V": "old"},
		map[string]any{"DiscDate": "2025-02-06", "DiscTime": "15:00", "Code": "A", "DiscNo": "2", "V": "new"},
		map[string]any{"DiscDate": "2025-03-06", "DiscTime": "15:00", "Code": "A", "DiscNo": "3", "V": "future"},
		map[string]any{"DiscDate": "2025-01-20", "DiscTime": "15:00", "Code": "B", "DiscNo": "4", "V": "b"},
	)
	cut, _ := time.Parse("2006-01-02", "2025-02-20")
	got := AsOf(f, cut, "DiscDate", "Code")
	if got.Height() != 2 {
		t.Fatalf("銘柄ごとに 1 件になるはず: %d", got.Height())
	}
	for i := 0; i < got.Height(); i++ {
		if v := got.Get(i, "V"); v != nil && *v == "future" {
			t.Error("未来の行を拾ってはいけない")
		}
	}
}

func TestLedger(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenLedger(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ep := bars()
	base := time.Date(2025, 1, 6, 9, 0, 0, 0, time.UTC)
	for i, target := range []string{"2025-01-06", "2025-01-07"} {
		if err := l.Record(IngestRecord{
			Endpoint: ep.Path, Target: target, Source: "api",
			FetchedUTC: base.Add(time.Duration(i) * time.Hour),
			Rows:       10, Changed: 10, Digest: "d", RunID: "r",
		}); err != nil {
			t.Fatal(err)
		}
	}
	last, err := l.Last(ep, "2025-01-06")
	if err != nil || last == nil {
		t.Fatalf("Last: %v %v", last, err)
	}
	if !last.FetchedUTC.Equal(base) {
		t.Errorf("時刻が往復しない: %v", last.FetchedUTC)
	}
	if missing, _ := l.Last(ep, "1999-01-01"); missing != nil {
		t.Error("記録の無い対象は nil のはず")
	}
	targets, _ := l.Targets(ep)
	if len(targets) != 2 {
		t.Errorf("Targets = %v", targets)
	}
	history, _ := l.History(ep, 10)
	if len(history) != 2 || history[0].Target != "2025-01-07" {
		t.Errorf("History は新しい順のはず: %v", history)
	}
	summary, _ := l.Summary()
	if len(summary) != 1 || summary[0].Rows != 20 {
		t.Errorf("Summary = %+v", summary)
	}
}

func TestMonthIn(t *testing.T) {
	cases := map[string]string{
		"bulk:equities_bars_daily_202501.csv.gz": "2025-01",
		"path/to/x_201912.csv.gz":                "2019-12",
		"no-month.csv.gz":                        "",
	}
	for key, want := range cases {
		if got := monthIn(key); got != want {
			t.Errorf("monthIn(%q) = %q, want %q", key, got, want)
		}
	}
}
