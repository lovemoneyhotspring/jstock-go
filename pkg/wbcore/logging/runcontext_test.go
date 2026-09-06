package logging

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindRunContext(t *testing.T) {
	ResetRunContext()
	t.Cleanup(ResetRunContext)

	if got := CurrentRunID(context.Background()); got != "" {
		t.Fatalf("束ねる前の run_id = %q（空であるべき）", got)
	}

	ctx, runID := BindRunContext(context.Background(), map[string]any{"app": "accum", "env": "uat"})
	if len(runID) != 12 {
		t.Fatalf("run_id = %q（12 桁の 16 進）", runID)
	}
	if CurrentRunID(ctx) != runID {
		t.Errorf("context から引けない: %s", CurrentRunID(ctx))
	}
	// 記録系は context を受け取らない経路からも同じ ID を引ける
	if CurrentRunID(context.TODO()) != runID {
		t.Errorf("プロセスの現在の実行から引けない: %s", CurrentRunID(context.TODO()))
	}

	run := RunContextOf(ctx)
	if run == nil || run.Fields["app"] != "accum" {
		t.Fatalf("束ねた項目 = %v", run)
	}
}

func TestRunIDsDiffer(t *testing.T) {
	ResetRunContext()
	t.Cleanup(ResetRunContext)
	_, first := BindRunContext(context.Background(), nil)
	_, second := BindRunContext(context.Background(), nil)
	if first == second {
		t.Fatal("実行ごとに違う ID であるべき")
	}
}

func TestTimestamperRejectsUnknownZone(t *testing.T) {
	if _, err := Timestamper("Mars/Olympus"); err == nil {
		t.Fatal("未知の時間帯は早めに弾くべき")
	}
	stamp, err := Timestamper("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	// 表示には必ずオフセットが付く（UTC と取り違えないため）
	if got := stamp(); !strings.Contains(got, "+09:00") {
		t.Errorf("JST のはずが %s", got)
	}
}

func TestLoggerRedactsAndUsesTimezone(t *testing.T) {
	ClearSecrets()
	logger, err := NewLogger("accum", "uat", "run1", "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()

	if err := logger.SetTimezone("Mars/Olympus"); err == nil {
		t.Error("未知の時間帯は弾くべき")
	}
	if err := logger.SetTimezone("UTC"); err != nil {
		t.Fatal(err)
	}
	if logger.RunID() != "run1" {
		t.Errorf("RunID = %s", logger.RunID())
	}

	var buf strings.Builder
	logger.SetOutput(&buf)
	RegisterSecret("super-secret-value")
	logger.Info("test.code", "秘密は super-secret-value です")
	if strings.Contains(buf.String(), "super-secret-value") {
		t.Fatalf("秘密が漏れている: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Errorf("マスクされていない: %s", buf.String())
	}
}

func TestNewLoggerForRun(t *testing.T) {
	ResetRunContext()
	t.Cleanup(ResetRunContext)
	ctx, runID := BindRunContext(context.Background(), nil)
	logger, err := NewLoggerForRun(ctx, "accum", "uat", "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logger.Close()
	// ログと履歴の run_id が必ず一致すること
	if logger.RunID() != runID {
		t.Fatalf("logger の run_id = %s, want %s", logger.RunID(), runID)
	}
}

func TestSetVerifyMarksEveryRecord(t *testing.T) {
	ClearSecrets()
	dir := t.TempDir()
	// env=prod で実機検証することがあるので、env では切り分けられない
	logger, err := NewLogger("daytrade", "prod", "run1", "open", dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("daytrade.order", "検証の前に出た行")
	logger.SetVerify(true)
	logger.Info("daytrade.order", "検証で出た行")
	logger.Warn("daytrade.carry", "検証で出た持ち越しの警告")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "daytrade-prod.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("行数 = %d, want 3", len(lines))
	}
	verify := make([]bool, 0, 3)
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["env"] != "prod" {
			t.Errorf("env = %v", record["env"])
		}
		v, _ := record["verify"].(bool)
		verify = append(verify, v)
	}
	// 印を立てる前の行には付かず、以降は警告も含めて全行に付く
	if verify[0] {
		t.Error("SetVerify の前の行に印が付いた")
	}
	if !verify[1] || !verify[2] {
		t.Errorf("SetVerify の後の行に印が無い: %v", verify)
	}
}

func TestVerifyAbsentOnNormalRun(t *testing.T) {
	// 通常の実行では項目ごと出ない（既存のログの形を変えない）
	dir := t.TempDir()
	logger, err := NewLogger("daytrade", "prod", "run1", "open", dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("daytrade.order", "通常の行")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "daytrade-prod.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &record); err != nil {
		t.Fatal(err)
	}
	if _, ok := record["verify"]; ok {
		t.Errorf("通常の実行に verify が付いた: %s", raw)
	}
}
