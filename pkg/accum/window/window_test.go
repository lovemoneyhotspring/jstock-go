package window

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// jst は JST の時刻を組み立てる。
func jst(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, clock.Tokyo)
}

func TestAllowsWithinWindow(t *testing.T) {
	w := Default() // 14:00〜15:00

	cases := []struct {
		name   string
		moment time.Time
		want   bool
	}{
		// 2026-09-03 は木曜日。
		{"開始ちょうどは含む", jst(2026, 9, 3, 14, 0), true},
		{"時間内", jst(2026, 9, 3, 14, 30), true},
		{"終了直前", jst(2026, 9, 3, 14, 59), true},
		{"終了ちょうどは含まない", jst(2026, 9, 3, 15, 0), false},
		{"開始前", jst(2026, 9, 3, 13, 59), false},
		{"寄り付き", jst(2026, 9, 3, 9, 0), false},
		// 2026-09-05 は土曜、2026-09-06 は日曜。
		{"土曜は時間内でも不可", jst(2026, 9, 5, 14, 30), false},
		{"日曜は時間内でも不可", jst(2026, 9, 6, 14, 30), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Allows(tc.moment); got != tc.want {
				t.Errorf("Allows(%s) = %v, want %v", tc.moment.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestAllowsDisabledIgnoresEverything(t *testing.T) {
	w := Unrestricted()
	// 制限なしなら土曜の深夜でも許す。
	if !w.Allows(jst(2026, 9, 5, 3, 0)) {
		t.Error("Enabled=false のとき Allows は常に true であるべき")
	}
}

func TestAllowsAcceptsUTCInput(t *testing.T) {
	w := Default()
	// 2026-09-03 05:30 UTC = 14:30 JST（木曜）。
	utc := time.Date(2026, 9, 3, 5, 30, 0, 0, time.UTC)
	if !w.Allows(utc) {
		t.Error("UTC 入力を JST に変換して判定すべき")
	}
}

func TestNextOpen(t *testing.T) {
	w := Default()

	t.Run("時間内ならその時刻自身", func(t *testing.T) {
		now := jst(2026, 9, 3, 14, 30)
		if got := w.NextOpen(now); !got.Equal(now) {
			t.Errorf("NextOpen = %v, want %v", got, now)
		}
	})

	t.Run("開始前なら当日の開始時刻", func(t *testing.T) {
		got := w.NextOpen(jst(2026, 9, 3, 10, 0))
		want := jst(2026, 9, 3, 14, 0)
		if !got.Equal(want) {
			t.Errorf("NextOpen = %v, want %v", got, want)
		}
	})

	t.Run("終了後なら翌日の開始時刻", func(t *testing.T) {
		got := w.NextOpen(jst(2026, 9, 3, 16, 0))
		want := jst(2026, 9, 4, 14, 0)
		if !got.Equal(want) {
			t.Errorf("NextOpen = %v, want %v", got, want)
		}
	})
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		w       TradingWindow
		wantErr bool
	}{
		{"既定", Default(), false},
		{"前場", TradingWindow{TimeOfDay{9, 0}, TimeOfDay{11, 30}, true}, false},
		{"大引けちょうど", TradingWindow{TimeOfDay{15, 0}, TimeOfDay{15, 30}, true}, false},
		{"開始が終了以降", TradingWindow{TimeOfDay{15, 0}, TimeOfDay{14, 0}, true}, true},
		{"開始と終了が同じ", TradingWindow{TimeOfDay{14, 0}, TimeOfDay{14, 0}, true}, true},
		{"開始が昼休み", TradingWindow{TimeOfDay{12, 0}, TimeOfDay{15, 0}, true}, true},
		{"終了が立会後", TradingWindow{TimeOfDay{14, 0}, TimeOfDay{16, 0}, true}, true},
		{"開始が寄り前", TradingWindow{TimeOfDay{8, 0}, TimeOfDay{11, 0}, true}, true},
		{"制限なしなら検査しない", TradingWindow{TimeOfDay{3, 0}, TimeOfDay{1, 0}, false}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.w.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParse(t *testing.T) {
	t.Run("nil は既定", func(t *testing.T) {
		w, err := Parse(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !w.Enabled || w.Start != DefaultStart || w.End != DefaultEnd {
			t.Errorf("Parse(nil) = %+v, want default", w)
		}
	})

	t.Run("false は制限なし", func(t *testing.T) {
		w, err := Parse(false)
		if err != nil {
			t.Fatal(err)
		}
		if w.Enabled {
			t.Error("Parse(false) は Enabled=false であるべき")
		}
	})

	t.Run("テーブル指定", func(t *testing.T) {
		w, err := Parse(map[string]any{"start": "09:30", "end": "11:00"})
		if err != nil {
			t.Fatal(err)
		}
		if w.Start != (TimeOfDay{9, 30}) || w.End != (TimeOfDay{11, 0}) {
			t.Errorf("Parse = %+v", w)
		}
	})

	t.Run("未知のキーは拒否", func(t *testing.T) {
		if _, err := Parse(map[string]any{"begin": "09:30"}); err == nil {
			t.Error("未知のキーはエラーにすべき")
		}
	})

	t.Run("不正な時刻形式は拒否", func(t *testing.T) {
		if _, err := Parse(map[string]any{"start": "9時"}); err == nil {
			t.Error("不正な形式はエラーにすべき")
		}
	})

	t.Run("立会時間外は拒否", func(t *testing.T) {
		if _, err := Parse(map[string]any{"start": "16:00", "end": "17:00"}); err == nil {
			t.Error("立会時間外はエラーにすべき")
		}
	})

	t.Run("非対応の型は拒否", func(t *testing.T) {
		if _, err := Parse(42); err == nil {
			t.Error("非対応の型はエラーにすべき")
		}
	})
}

func TestDescribe(t *testing.T) {
	if got := Default().Describe(); got != "14:00〜15:00 JST" {
		t.Errorf("Describe() = %q", got)
	}
	if got := Unrestricted().Describe(); got != "制限なし" {
		t.Errorf("Describe() = %q", got)
	}
}
