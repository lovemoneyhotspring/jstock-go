package logging

import (
	"context"
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
	if CurrentRunID(nil) != runID {
		t.Errorf("プロセスの現在の実行から引けない: %s", CurrentRunID(nil))
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
