package output

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

type fakeFrame struct{ rows []map[string]any }

func (f fakeFrame) ToMaps() []map[string]any { return f.rows }

func TestEncodeMoneyStaysString(t *testing.T) {
	// 金額は円未満の誤差が出ないよう必ず文字列
	if got := Encode(decimal.RequireFromString("1234.56")); got != "1234.56" {
		t.Errorf("Decimal = %v (%T)", got, got)
	}
}

func TestEncodeTimes(t *testing.T) {
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if got := Encode(day); got != "2026-09-03" {
		t.Errorf("日付 = %v", got)
	}
	moment := time.Date(2026, 9, 3, 12, 30, 0, 0, time.UTC)
	if got := Encode(moment); !strings.HasPrefix(got.(string), "2026-09-03T12:30:00") {
		t.Errorf("時刻 = %v", got)
	}
}

func TestEncodeSetIsSorted(t *testing.T) {
	got := Encode(map[string]struct{}{"b": {}, "a": {}})
	list, ok := got.([]string)
	if !ok || len(list) != 2 || list[0] != "a" {
		t.Fatalf("集合 = %v", got)
	}
}

func TestDumpSortsKeys(t *testing.T) {
	text, err := Dump(map[string]any{"b": 1, "a": 2})
	if err != nil {
		t.Fatal(err)
	}
	if text != `{"a":2,"b":1}` {
		t.Errorf("鍵が辞書順でない: %s", text)
	}
}

func TestRowsOfEncodesValues(t *testing.T) {
	frame := fakeFrame{rows: []map[string]any{
		{"symbol": "7203", "amount": decimal.NewFromInt(1000)},
	}}
	rows := RowsOf(frame)
	if len(rows) != 1 || rows[0]["amount"] != "1000" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestEmitJSONWritesOneLine(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitJSONTo(&buf, map[string]any{"ok": true, "rows": []any{}}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "{\"ok\":true,\"rows\":[]}\n" {
		t.Errorf("出力 = %q", buf.String())
	}
}

func TestEmitErrorAlwaysHasOk(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitErrorTo(&buf, "設定がありません", ""); err != nil {
		t.Fatal(err)
	}
	// 呼ぶ側が成否を 1 つの鍵で判定できること
	if !strings.Contains(buf.String(), `"ok":false`) || !strings.Contains(buf.String(), `"error":"error"`) {
		t.Errorf("出力 = %s", buf.String())
	}
}
