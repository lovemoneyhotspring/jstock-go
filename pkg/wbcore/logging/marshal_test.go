package logging

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

// 直列化できない extra（NaN）でも行を捨てず、失敗の分かる最小の行を書く。
// 各行は 1 回の write で改行まで含むので、行数と JSON がそろう
func TestLogKeepsMinimalLineOnMarshalError(t *testing.T) {
	ClearSecrets()
	logger, err := NewLogger("accum", "uat", "run1", "test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	logger.SetOutput(nil)
	logger.Info("test.nan", "NaN を含む", map[string]any{"v": math.NaN()})
	logger.Info("test.ok", "通常の行")
	path := logger.file.Name()
	logger.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("行数 = %d, want 2: %q", len(lines), raw)
	}
	var rec LogRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("JSON が壊れている: %v", err)
	}
	if rec.Level != "info" || rec.Code != "test.nan" || rec.Msg != "NaN を含む" {
		t.Errorf("level・code・msg が残っていない: %+v", rec)
	}
	if _, has := rec.Extra["marshal_error"]; !has {
		t.Errorf("失敗の理由が無い: %+v", rec)
	}
	if _, has := rec.Extra["v"]; has {
		t.Errorf("直列化できない項目が残っている: %+v", rec)
	}
}
