package history

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func day(text string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", text, time.UTC)
	if err != nil {
		panic(err)
	}
	return d
}

func sampleFrame() Frame {
	return NewFrame(
		[]Column{
			{Name: "symbol", Type: TypeString},
			{Name: "score", Type: TypeFloat64},
			{Name: "rank", Type: TypeInt64},
			{Name: "picked", Type: TypeBool},
		},
		[]map[string]any{
			{"symbol": "7203", "score": 1.5, "rank": int64(1), "picked": true},
			{"symbol": "6758", "score": 0.5, "rank": int64(2), "picked": false},
		},
	)
}

func TestAppendAndRead(t *testing.T) {
	store := NewStore(t.TempDir())
	at := time.Date(2026, 9, 3, 1, 2, 3, 0, time.UTC)

	path, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "run1", At: at})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !strings.HasSuffix(path, "2026-09-03T010203Z-run1.parquet") {
		t.Fatalf("想定外のファイル名: %s", path)
	}

	frame, err := store.Read("plan", Range{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d, want 2", frame.Height())
	}
	// 鍵の列が先頭に来ていること
	names := frame.Names()
	for i, want := range KeyColumns {
		if names[i] != want {
			t.Fatalf("names[%d] = %s, want %s", i, names[i], want)
		}
	}
	row := frame.SortBy([]string{"rank"}, nil).Rows[0]
	if row["symbol"] != "7203" {
		t.Errorf("symbol = %v", row["symbol"])
	}
	if row["score"] != 1.5 {
		t.Errorf("score = %v (%T)", row["score"], row["score"])
	}
	if row["picked"] != true {
		t.Errorf("picked = %v", row["picked"])
	}
	if row["run_id"] != "run1" {
		t.Errorf("run_id = %v", row["run_id"])
	}
	if d, ok := row["day"].(time.Time); !ok || !d.Equal(day("2026-09-03")) {
		t.Errorf("day = %v", row["day"])
	}
	if ts, ok := row["recorded_at"].(time.Time); !ok || !ts.Equal(at) {
		t.Errorf("recorded_at = %v, want %v", row["recorded_at"], at)
	}
}

func TestAppendNeverOverwrites(t *testing.T) {
	store := NewStore(t.TempDir())
	at := time.Date(2026, 9, 3, 1, 2, 3, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "run1", At: at}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	files := store.Files("plan", Range{})
	if len(files) != 3 {
		t.Fatalf("ファイル数 = %d, want 3（同名は枝番で残す）", len(files))
	}
	frame, err := store.Read("plan", Range{})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 6 {
		t.Fatalf("行数 = %d, want 6", frame.Height())
	}
}

func TestEmptyFrameIsRecorded(t *testing.T) {
	store := NewStore(t.TempDir())
	empty := NewFrame([]Column{{Name: "symbol", Type: TypeString}}, nil)
	if _, err := store.Append("plan", empty, day("2026-09-03"), AppendOptions{RunID: "run0"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 「その日は条件に合う銘柄が無かった」も記録のうち——ファイルは残る
	if len(store.Files("plan", Range{})) != 1 {
		t.Fatal("0 行でもファイルは残るべき")
	}
	frame, err := store.Read("plan", Range{})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 0 {
		t.Fatalf("行数 = %d, want 0", frame.Height())
	}
}

func TestReadWindowAndDays(t *testing.T) {
	store := NewStore(t.TempDir())
	for _, d := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
		if _, err := store.Append("plan", sampleFrame(), day(d), AppendOptions{RunID: "r" + d}); err != nil {
			t.Fatal(err)
		}
	}
	days := store.Days("plan")
	if len(days) != 3 || !days[0].Equal(day("2026-09-01")) {
		t.Fatalf("days = %v", days)
	}
	frame, err := store.Read("plan", Range{Start: day("2026-09-02"), End: day("2026-09-03")})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 4 {
		t.Fatalf("行数 = %d, want 4", frame.Height())
	}
}

func TestLatestPicksLastRun(t *testing.T) {
	store := NewStore(t.TempDir())
	base := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "early", At: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "late", At: base.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	frame, err := store.Latest("plan", day("2026-09-03"))
	if err != nil {
		t.Fatal(err)
	}
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d, want 2", frame.Height())
	}
	for _, row := range frame.Rows {
		if row["run_id"] != "late" {
			t.Fatalf("run_id = %v, want late", row["run_id"])
		}
	}
}

// 列が後から増えても、古いファイルはそのまま読めること（前方互換）。
func TestReadUnionsDifferentColumns(t *testing.T) {
	store := NewStore(t.TempDir())
	old := NewFrame([]Column{{Name: "symbol", Type: TypeString}}, []map[string]any{{"symbol": "7203"}})
	if _, err := store.Append("plan", old, day("2026-09-01"), AppendOptions{RunID: "old"}); err != nil {
		t.Fatal(err)
	}
	newer := NewFrame(
		[]Column{{Name: "symbol", Type: TypeString}, {Name: "score", Type: TypeFloat64}},
		[]map[string]any{{"symbol": "6758", "score": 2.0}},
	)
	if _, err := store.Append("plan", newer, day("2026-09-02"), AppendOptions{RunID: "new"}); err != nil {
		t.Fatal(err)
	}
	frame, err := store.Read("plan", Range{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if frame.Height() != 2 {
		t.Fatalf("行数 = %d, want 2", frame.Height())
	}
	if !frame.Has("score") {
		t.Fatal("後から増えた列が読めていない")
	}
	sorted := frame.SortBy([]string{"day"}, nil)
	if sorted.Rows[0]["score"] != nil {
		t.Errorf("古い行の score は null であるべき: %v", sorted.Rows[0]["score"])
	}
}

func TestSummary(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-01"), AppendOptions{RunID: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-05"), AppendOptions{RunID: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append("review", sampleFrame(), day("2026-09-02"), AppendOptions{RunID: "c"}); err != nil {
		t.Fatal(err)
	}
	summary := store.Summary()
	if len(summary) != 2 {
		t.Fatalf("種類 = %d, want 2", len(summary))
	}
	if summary[0].Kind != "plan" || summary[0].Files != 2 {
		t.Fatalf("summary[0] = %+v", summary[0])
	}
	if !summary[0].FirstDay.Equal(day("2026-09-01")) || !summary[0].LastDay.Equal(day("2026-09-05")) {
		t.Fatalf("期間が違う: %v 〜 %v", summary[0].FirstDay, summary[0].LastDay)
	}
}

func TestShowJSON(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Show(&buf, store, "plan", ShowOptions{AsJSON: true}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("JSON が壊れている: %v\n%s", err, buf.String())
	}
	if payload["ok"] != true || payload["count"] != float64(2) {
		t.Fatalf("payload = %v", payload)
	}
	rows, ok := payload["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %v", payload["rows"])
	}
	// 種類の一覧（kind 省略）も JSON で出せること
	buf.Reset()
	if err := Show(&buf, store, "", ShowOptions{AsJSON: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"kind":"plan"`) {
		t.Fatalf("種類の一覧が出ていない: %s", buf.String())
	}
}

func TestWriteCSV(t *testing.T) {
	store := NewStore(t.TempDir())
	if _, err := store.Append("plan", sampleFrame(), day("2026-09-03"), AppendOptions{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	frame, err := store.Read("plan", Range{})
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/out/plan.csv"
	if err := WriteCSV(path, frame); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Show(&buf, store, "plan", ShowOptions{CSVPath: path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "2 行を") {
		t.Fatalf("CSV 書き出しの案内が出ていない: %s", buf.String())
	}
}
