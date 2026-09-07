package archive

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Window は 1 日の中の時間帯（JST）。From を含み To を含まない半開区間で、
// 時刻の文字列（"09:00" や "09:00:00.065599"）を辞書順で比べる。
//
// "HH:MM" と "HH:MM:SS.ffffff" は先頭が揃っているので、そのまま比べられる。
// From="09:00", To="09:10" なら 09:00:00.000000 〜 09:09:59.999999 が残る。
// 引けの 15:30:00.xxx を残すには To を "15:31" にする。
type Window struct {
	From, To string
}

// Windows は時間帯の集まり。空なら「絞らない」（全時間帯を残す）。
type Windows []Window

// ParseWindows は "09:00-09:10,15:10-15:31" を読む。空文字は絞らない。
func ParseWindows(spec string) (Windows, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out Windows
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "-")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || !validClock(from) || !validClock(to) || from >= to {
			return nil, fmt.Errorf("時間帯 %q を読めません（HH:MM-HH:MM。開始 < 終了、終了は含まない）", part)
		}
		out = append(out, Window{From: from, To: to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out, nil
}

// validClock は "HH:MM" か。
func validClock(s string) bool {
	if len(s) != 5 || s[2] != ':' {
		return false
	}
	for i, c := range s {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return s[:2] <= "23" && s[3:] <= "59"
}

// Keep は時刻の文字列がどれかの時間帯に入るか。絞らない設定なら常に真。
func (w Windows) Keep(clock string) bool {
	if len(w) == 0 {
		return true
	}
	for _, win := range w {
		if clock >= win.From && clock < win.To {
			return true
		}
	}
	return false
}

// String は ParseWindows が読める形に戻す。
func (w Windows) String() string {
	parts := make([]string, 0, len(w))
	for _, win := range w {
		parts = append(parts, win.From+"-"+win.To)
	}
	return strings.Join(parts, ",")
}

// Windows は端点に設定された時間帯（環境変数 WindowEnv から）。
// 時刻の列（TimeColumn）が無い端点や、環境変数が空なら絞らない。
// 読めない値は黙って全部残すのではなくエラーにする（絞るつもりで絞れていないのが最悪）。
func (e Endpoint) Windows() (Windows, error) {
	if e.TimeColumn == "" || e.WindowEnv == "" {
		return nil, nil
	}
	return ParseWindows(os.Getenv(e.WindowEnv))
}

// trimWindows は Frame から時間帯の外の行を落とす。落とした行数を返す。
// 絞らない設定なら何もしない。
func trimWindows(f *Frame, ep Endpoint, w Windows) int {
	if len(w) == 0 || f == nil || f.Height() == 0 {
		return 0
	}
	idx := f.col(ep.TimeColumn)
	if idx < 0 {
		return 0
	}
	kept := f.Rows[:0]
	for _, row := range f.Rows {
		if v := cell(row, idx); v != nil && w.Keep(*v) {
			kept = append(kept, row)
		}
	}
	dropped := len(f.Rows) - len(kept)
	f.Rows = kept
	return dropped
}
