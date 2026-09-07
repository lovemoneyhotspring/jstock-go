package archive

import (
	"os"
	"testing"
)

func TestParseWindows(t *testing.T) {
	w, err := ParseWindows(" 15:10-15:31, 09:00-09:10 ")
	if err != nil {
		t.Fatal(err)
	}
	if w.String() != "09:00-09:10,15:10-15:31" {
		t.Errorf("並びが開始時刻順になっていない: %s", w)
	}
	// 半開区間。分足の "HH:MM" もティックの "HH:MM:SS.ffffff" も同じ比較で判定できる
	cases := map[string]bool{
		"09:00":           true,
		"09:00:00.065599": true,
		"09:09:59.999999": true,
		"09:10":           false,
		"09:10:00.000001": false,
		"15:09:59.000000": false,
		"15:30:00.123456": true, // 引け。To を 15:31 にしているので入る
		"15:31":           false,
		"":                false,
	}
	for clock, want := range cases {
		if got := w.Keep(clock); got != want {
			t.Errorf("Keep(%q) = %v, want %v", clock, got, want)
		}
	}
	// 空は「絞らない」
	none, err := ParseWindows("")
	if err != nil || len(none) != 0 || !none.Keep("03:00") {
		t.Errorf("空の指定が全部残す扱いになっていない: %v %v", none, err)
	}
	for _, bad := range []string{"9:00-9:10", "09:10-09:00", "09:00", "09:00-24:00", "09:00-09:60"} {
		if _, err := ParseWindows(bad); err == nil {
			t.Errorf("%q を受け入れている", bad)
		}
	}
}

func TestEndpointWindowsRejectsBadEnv(t *testing.T) {
	t.Setenv(TicksWindowsEnv, "junk")
	if _, err := ticks().Windows(); err == nil {
		t.Error("読めない時間帯を黙って全部残す扱いにしている")
	}
	// 時刻の列が無い端点は環境変数があっても絞らない
	t.Setenv(TicksWindowsEnv, "09:00-09:10")
	if w, err := bars().Windows(); err != nil || len(w) != 0 {
		t.Errorf("日足に時間帯が付いた: %v %v", w, err)
	}
	os.Unsetenv(TicksWindowsEnv)
}
