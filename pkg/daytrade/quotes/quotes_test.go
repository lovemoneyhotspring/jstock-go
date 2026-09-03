package quotes

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/shopspring/decimal"
)

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quotes.csv")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCSVFetch(t *testing.T) {
	path := writeCSV(t, "symbol,price,at\n7203,2500,2026-09-03T00:00:00Z\n9984,8000\n1234,-1\n")
	source := &CSV{Path: path}
	got, err := source.Fetch([]string{"7203", "9984", "1234", "5555"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("気配 %d 件, want 2（負の値と未収載は落ちる）: %v", len(got), got)
	}
	if !got["7203"].Price.Equal(decimal.NewFromInt(2500)) {
		t.Errorf("価格 = %s", got["7203"].Price)
	}
	if got["7203"].At.Format(time.RFC3339) != "2026-09-03T00:00:00Z" {
		t.Errorf("時刻を読めていない: %v", got["7203"].At)
	}
	// at が無ければ「今」（鮮度の検査は通る）
	if got["9984"].At.IsZero() {
		t.Error("at 省略時に時刻が入っていない")
	}
	if got["7203"].Source != "csv" {
		t.Errorf("取得元 = %s", got["7203"].Source)
	}
}

func TestCSVMissingFile(t *testing.T) {
	source := &CSV{Path: filepath.Join(t.TempDir(), "none.csv")}
	if _, err := source.Fetch([]string{"7203"}); err == nil {
		t.Error("ファイルが無いのにエラーにならない")
	}
}

func TestFreshDropsStaleAndDelayed(t *testing.T) {
	now := time.Date(2026, 9, 3, 0, 5, 0, 0, time.UTC)
	received := map[string]selection.Quote{
		"fresh":   {Symbol: "fresh", At: now.Add(-30 * time.Second)},
		"stale":   {Symbol: "stale", At: now.Add(-10 * time.Minute)},
		"delayed": {Symbol: "delayed", At: now, Delayed: true},
	}
	kept, stale, delayed := Fresh(received, 90, now, false)
	if len(kept) != 1 || kept["fresh"].Symbol != "fresh" {
		t.Errorf("残った気配 = %v", kept)
	}
	if len(stale) != 1 || len(delayed) != 1 {
		t.Errorf("stale=%v delayed=%v", stale, delayed)
	}
	// 検証用の逃げ道: allow_delayed なら全部通す
	kept, _, _ = Fresh(received, 90, now, true)
	if len(kept) != 3 {
		t.Errorf("allow_delayed で %d 件しか通っていない", len(kept))
	}
}

func TestNewRejectsUnknownSource(t *testing.T) {
	if _, err := New("yahoo", Params{}); err == nil {
		t.Error("未知の quote_source が通る")
	}
	if _, err := New("csv", Params{}); err == nil {
		t.Error("quote_file 無しの csv が通る")
	}
	source, err := New("csv", Params{QuoteFile: "a.csv"})
	if err != nil || source.Name() != "csv" {
		t.Errorf("csv を組み立てられない: %v", err)
	}
}
