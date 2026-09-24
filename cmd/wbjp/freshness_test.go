package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestBarsUnusable は W6。場中の run では前営業日の足が最新であるべきで、それより古い・
// 読めない・無い足では判断しない。連休はカレンダーで数える。
func TestBarsUnusable(t *testing.T) {
	// 2026-09-19〜23 は休場（シルバーウィーク）
	cal := calendar.New([]time.Time{day("2026-09-17"), day("2026-09-18"), day("2026-09-24"), day("2026-09-25")})
	bars := func(last string) []domain.Bar { return []domain.Bar{{Date: "2026-09-10"}, {Date: last}} }

	cases := []struct {
		name  string
		bars  []domain.Bar
		err   error
		today string
		want  string // 空なら判断してよい
	}{
		{"連休明けは連休前の足で足りる", bars("2026-09-18"), nil, "2026-09-24", ""},
		{"前営業日の足が無い", bars("2026-09-17"), nil, "2026-09-24", "足が古い"},
		{"当日の足があってもよい", bars("2026-09-24"), nil, "2026-09-24", ""},
		{"翌日は当日の足が要る", bars("2026-09-18"), nil, "2026-09-25", "足が古い"},
		{"読めない", nil, errors.New("parquet が壊れている"), "2026-09-24", "足を読めない"},
		{"無い", nil, nil, "2026-09-24", "足が無い"},
		{"日付が読めない", bars("20260918"), nil, "2026-09-24", "日付"},
	}
	for _, c := range cases {
		got := barsUnusable(c.bars, c.err, day(c.today), cal.PreviousTradingDay)
		if c.want == "" && got != "" {
			t.Errorf("%s: 判断してよいはず: %q", c.name, got)
		}
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: %q を含むはず: %q", c.name, c.want, got)
		}
	}
}
