package digest

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
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

func TestFlushKeepsMinimalLineOnMarshalError(t *testing.T) {
	dir := t.TempDir()
	start(t, dir)
	Note(map[string]any{"ratio": math.NaN()})

	if err := Flush(); err == nil {
		t.Fatal("直列化の失敗はエラーで返すべき")
	}
	record := readOnly(t, dir)
	if record["run_id"] != "abc123" || record["outcome"] != "ok" {
		t.Fatalf("固定項目が残っていない: %v", record)
	}
	if _, has := record["marshal_error"]; !has {
		t.Errorf("失敗の理由が無い: %v", record)
	}
	if _, has := record["ratio"]; has {
		t.Errorf("直列化できない項目が残っている: %v", record)
	}
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

// ダイジェストの日付は JST。UTC で切ると 09:00 JST より前の実行が前日のファイルに入る。
func TestDayOfIsJST(t *testing.T) {
	// 2026-09-13 21:30 UTC = 2026-09-14 06:30 JST（night-repair の時間帯）
	if got := DayOf(time.Date(2026, 9, 13, 21, 30, 0, 0, time.UTC)); got != "2026-09-14" {
		t.Errorf("DayOf = %q, want 2026-09-14（JST）", got)
	}
	// 2026-09-14 14:59 UTC = 23:59 JST はまだ同じ日
	if got := DayOf(time.Date(2026, 9, 14, 14, 59, 0, 0, time.UTC)); got != "2026-09-14" {
		t.Errorf("DayOf = %q, want 2026-09-14", got)
	}
	// 15:00 UTC = 翌 0:00 JST
	if got := DayOf(time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)); got != "2026-09-15" {
		t.Errorf("DayOf = %q, want 2026-09-15", got)
	}
}

// Day を省いたときの書き出し先は JST の今日。
func TestStartRunDefaultsToJSTDay(t *testing.T) {
	dir := t.TempDir()
	Reset()
	t.Cleanup(Reset)
	StartRun(StartOptions{App: "accum", Env: "uat", Command: "run", RunID: "r", StateDir: dir})
	want := Path(dir, "uat", DayOf(clock.NowUTC()))
	mu.Lock()
	got := current.path
	mu.Unlock()
	if got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
}

func TestSetVerifyMarksRun(t *testing.T) {
	dir := t.TempDir()
	start(t, dir)
	SetVerify(true)
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	record := readOnly(t, dir)
	if v, _ := record["verify"].(bool); !v {
		t.Errorf("検証の印が無い: %v", record)
	}
}

func TestVerifyAbsentByDefault(t *testing.T) {
	// 通常の実行では項目ごと出ない（既存のダイジェストの形を変えない）
	dir := t.TempDir()
	start(t, dir)
	if err := Flush(); err != nil {
		t.Fatal(err)
	}
	if _, ok := readOnly(t, dir)["verify"]; ok {
		t.Error("通常の実行に verify が付いた")
	}
}
