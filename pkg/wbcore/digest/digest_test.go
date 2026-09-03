package digest

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func start(t *testing.T, stateDir string) {
	t.Helper()
	Reset()
	StartRun(StartOptions{App: "accum", Env: "uat", Command: "run", RunID: "abc123", StateDir: stateDir, Day: "2026-09-03"})
	t.Cleanup(Reset)
}

func readOnly(t *testing.T, stateDir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(Path(stateDir, "uat", "2026-09-03"))
	if err != nil {
		t.Fatalf("ダイジェストが書かれていない: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("行数 = %d, want 1", len(lines))
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("JSON が壊れている: %v", err)
	}
	return record
}

func TestFlushWritesOneLine(t *testing.T) {
	dir := t.TempDir()
	start(t, dir)
	Note(map[string]any{"candidates": 12})
	Add(map[string]int{"placed": 1})
	Add(map[string]int{"placed": 2})

	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	record := readOnly(t, dir)
	if record["schema"] != Schema || record["app"] != "accum" || record["run_id"] != "abc123" {
		t.Fatalf("固定項目が足りない: %v", record)
	}
	if record["outcome"] != "ok" {
		t.Errorf("outcome = %v", record["outcome"])
	}
	if record["candidates"] != float64(12) || record["placed"] != float64(3) {
		t.Errorf("項目が平らに並んでいない: %v", record)
	}
	if _, has := record["anomalies"]; has {
		t.Errorf("異常が無いのに anomalies が付いている")
	}

	// 2 回目の Flush は何もしない（行が増えない）
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	readOnly(t, dir)
}

func TestAnomalyAndFail(t *testing.T) {
	dir := t.TempDir()
	start(t, dir)
	Anomaly("quote.stale", "7203")
	Fail("broker.error", "接続できません")
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	record := readOnly(t, dir)
	if record["outcome"] != "error" {
		t.Errorf("outcome = %v, want error", record["outcome"])
	}
	anomalies, ok := record["anomalies"].([]any)
	if !ok || len(anomalies) != 2 {
		t.Fatalf("anomalies = %v", record["anomalies"])
	}
	if anomalies[0] != "quote.stale: 7203" {
		t.Errorf("anomalies[0] = %v", anomalies[0])
	}
}

func TestSkippedIsNotAnomaly(t *testing.T) {
	dir := t.TempDir()
	start(t, dir)
	Skipped("発注時間帯の外")
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	record := readOnly(t, dir)
	if record["outcome"] != "skip" {
		t.Errorf("outcome = %v", record["outcome"])
	}
	if record["reason"] != "発注時間帯の外" {
		t.Errorf("reason = %v", record["reason"])
	}
	if _, has := record["anomalies"]; has {
		t.Errorf("休日・時間帯外は異常ではない")
	}
}

func TestAppendsWithoutClobbering(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"run1", "run2"} {
		Reset()
		StartRun(StartOptions{App: "accum", Env: "uat", Command: "run", RunID: id, StateDir: dir, Day: "2026-09-03"})
		if err := Flush(); err != nil {
			t.Fatal(err)
		}
	}
	Reset()
	raw, err := os.ReadFile(Path(dir, "uat", "2026-09-03"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(raw)), "\n"); len(lines) != 2 {
		t.Fatalf("行数 = %d, want 2（実行ごとに 1 行足す）", len(lines))
	}
}

// StartRun 前の呼び出しは黙って無視する（記録の付帯物で実行を落とさない）。
func TestNoRunIsHarmless(t *testing.T) {
	Reset()
	Note(map[string]any{"a": 1})
	Add(map[string]int{"b": 1})
	Anomaly("x", "")
	Fail("y", "")
	Skipped("z")
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
}
