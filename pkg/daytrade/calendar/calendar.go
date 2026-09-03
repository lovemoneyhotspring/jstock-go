// Package calendar は東証の営業日。J-Quants の取引カレンダー
// （HolDiv: 1=営業日, 2=半日, 0/3=休場）を使う。
package calendar

import (
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// TradingDivisions は営業日とみなす HolDiv の値。
var TradingDivisions = map[string]bool{"1": true, "2": true}

// Calendar は営業日の並び。カレンダーが無い期間は平日で代用する。
type Calendar struct {
	days []time.Time
	set  map[string]bool
}

// New は日付の並びからカレンダーを作る（重複と順序は内部で整える）。
func New(days []time.Time) *Calendar {
	set := make(map[string]bool, len(days))
	uniq := make([]time.Time, 0, len(days))
	for _, d := range days {
		day := d.UTC().Truncate(24 * time.Hour)
		key := day.Format("2006-01-02")
		if set[key] {
			continue
		}
		set[key] = true
		uniq = append(uniq, day)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Before(uniq[j]) })
	return &Calendar{days: uniq, set: set}
}

// FromArchive は保存済みの取引カレンダーから営業日を読む。
// カレンダーが無ければ空のカレンダー（＝平日で代用）を返す。
func FromArchive(arch *archive.Archive) *Calendar {
	frame, err := arch.Scan(archive.CalendarEndpoint())
	if err != nil || frame == nil || frame.Height() == 0 || !frame.HasColumn("HolDiv") {
		return New(nil)
	}
	var days []time.Time
	for i := range frame.Rows {
		div := frame.Get(i, "HolDiv")
		date := frame.Get(i, "Date")
		if div == nil || date == nil || !TradingDivisions[*div] {
			continue
		}
		d, err := time.Parse("2006-01-02", *date)
		if err != nil {
			continue
		}
		days = append(days, d)
	}
	return New(days)
}

// Empty はカレンダーを 1 日も持たないか。
func (c *Calendar) Empty() bool { return len(c.days) == 0 }

// IsTradingDay は営業日か。カレンダーの範囲外は平日で代用する
// （範囲外で「休場」と答えると、まだ取り込んでいない先の日付を全部止めてしまう）。
func (c *Calendar) IsTradingDay(day time.Time) bool {
	day = normalize(day)
	if c.Empty() || day.Before(c.days[0]) || day.After(c.days[len(c.days)-1]) {
		wd := day.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	}
	return c.set[day.Format("2006-01-02")]
}

// NextTradingDay は after の次の営業日。inclusive なら after 自身も候補。
func (c *Calendar) NextTradingDay(after time.Time, inclusive bool) (time.Time, error) {
	day := normalize(after)
	if !inclusive {
		day = day.AddDate(0, 0, 1)
	}
	for range 60 {
		if c.IsTradingDay(day) {
			return day, nil
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("%s 以降 60 日に営業日がありません", normalize(after).Format("2006-01-02"))
}

// PreviousTradingDay は before の前の営業日（before 自身は含まない）。
func (c *Calendar) PreviousTradingDay(before time.Time) (time.Time, error) {
	day := normalize(before).AddDate(0, 0, -1)
	for range 60 {
		if c.IsTradingDay(day) {
			return day, nil
		}
		day = day.AddDate(0, 0, -1)
	}
	return time.Time{}, fmt.Errorf("%s 以前 60 日に営業日がありません", normalize(before).Format("2006-01-02"))
}

// PreviousTradingDays は day より前の営業日を n 日ぶん（新しい順）。
func (c *Calendar) PreviousTradingDays(day time.Time, n int) ([]time.Time, error) {
	out := make([]time.Time, 0, n)
	cursor := normalize(day)
	for range n {
		prev, err := c.PreviousTradingDay(cursor)
		if err != nil {
			return out, err
		}
		out = append(out, prev)
		cursor = prev
	}
	return out, nil
}

// Days は保持している営業日（昇順）。
func (c *Calendar) Days() []time.Time { return c.days }

func normalize(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
