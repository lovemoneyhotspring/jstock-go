package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// 半日立会（HolDiv = 2）の日だけ open を見送る。ふつうの営業日・カレンダーに無い日は建てる側
// ——分からない日を半日と読んで黙って休まない。
func TestSkipHalfDayOnlyOnHalfDays(t *testing.T) {
	// 朝の通知の印を実際の state/ に作らない
	saved := appSettings.StateDir
	appSettings.StateDir = t.TempDir()
	t.Cleanup(func() { appSettings.StateDir = saved })
	s := func(v string) *string { return &v }
	arch := archive.NewArchive(t.TempDir())
	frame := &archive.Frame{Columns: []string{"Date", "HolDiv"}}
	for _, row := range [][2]string{{"2026-12-29", "1"}, {"2026-12-30", "2"}, {"2026-12-31", "0"}} {
		frame.AppendRow(map[string]*string{"Date": s(row[0]), "HolDiv": s(row[1])})
	}
	if _, err := arch.Upsert(archive.CalendarEndpoint(), frame); err != nil {
		t.Fatal(err)
	}
	cal := calendar.FromArchive(arch)
	cfg := dtconfig.Default()

	cases := []struct {
		day  string
		want bool
	}{
		{"2026-12-29", false}, // 終日立会
		{"2026-12-30", true},  // 半日立会
		{"2027-06-01", false}, // カレンダーの範囲外
	}
	for _, c := range cases {
		day, _ := time.Parse(DateLayout, c.day)
		if got := skipHalfDay(cal, cfg, day, "open"); got != c.want {
			t.Errorf("%s: skipHalfDay = %v, want %v", c.day, got, c.want)
		}
	}
	// カレンダーが空（平日で代用）でも半日とは読まない
	day, _ := time.Parse(DateLayout, "2026-12-30")
	if skipHalfDay(calendar.New(nil), cfg, day, "open") {
		t.Error("空のカレンダーで半日立会と読んだ")
	}
}

// 朝の通知は 1 日 1 回。印を作れないときは知らせる側に倒し、置き場が無ければ何もしない。
func TestFirstOfDayFiresOncePerDay(t *testing.T) {
	dir := t.TempDir()
	day, _ := time.Parse(DateLayout, "2026-12-30")
	next := day.AddDate(0, 0, 1)
	for i, c := range []struct {
		dir  string
		day  time.Time
		want bool
	}{
		{dir, day, true},  // その日の 1 回目
		{dir, day, false}, // 2 回目以降は黙る
		{dir, next, true}, // 日が変われば改めて知らせる
		{"", day, false},  // 置き場が無い（設定が読めていない）
		{filepath.Join(dir, "missing"), day, true}, // 印を作れない → 知らせる側
	} {
		if got := firstOfDay(c.dir, "half_day", c.day); got != c.want {
			t.Errorf("#%d: firstOfDay = %v, want %v", i, got, c.want)
		}
	}
}
