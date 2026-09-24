package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
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

// TestTradingDayGate は W7 に伴う休場日の扱い。cron を平日で回すので、祝日は run 自身が見送る。
// カレンダーが読めないときは発注する回だけ止める。
func TestTradingDayGate(t *testing.T) {
	cal := calendar.New([]time.Time{day("2026-09-18"), day("2026-09-24")})
	if skip, err := tradingDayGate(cal, day("2026-09-22"), true); err != nil || skip == "" {
		t.Errorf("平日の休場日は見送るはず: %q %v", skip, err)
	}
	if skip, err := tradingDayGate(cal, day("2026-09-24"), true); err != nil || skip != "" {
		t.Errorf("営業日は回すはず: %q %v", skip, err)
	}
	empty := calendar.New(nil)
	if _, err := tradingDayGate(empty, day("2026-09-24"), true); err == nil {
		t.Error("カレンダーが無いのに発注する回を止めなかった")
	}
	if skip, err := tradingDayGate(empty, day("2026-09-24"), false); err != nil || skip != "" {
		t.Errorf("dry-run はカレンダーが無くても続けるはず: %q %v", skip, err)
	}
}

// TestReportUnusableBarsAlertsHeld は、足が古い銘柄のうち保有中のもの（損切りも止まる）を
// 通知の対象として拾う。保有していない銘柄は通知しない（ログとダイジェストだけ）。
func TestReportUnusableBarsAlertsHeld(t *testing.T) {
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")
	unusable := map[string]string{"7203": "最後の足 2026-09-18", "6758": "足を読めない"}
	positions := map[string]domain.Position{
		"7203": {Symbol: "7203", Quantity: decimal.NewFromInt(100)},
		"6758": {Symbol: "6758", Quantity: decimal.Zero},
	}
	held := reportUnusableBars(unusable, positions, logger)
	if len(held) != 1 || !strings.HasPrefix(held[0], "7203（保有 100 株）") {
		t.Errorf("通知する保有銘柄: %v", held)
	}
	if got := reportUnusableBars(map[string]string{"6758": "x"}, positions, logger); len(got) != 0 {
		t.Errorf("保有していない銘柄だけなら通知しない: %v", got)
	}
	if got := reportUnusableBars(nil, positions, logger); got != nil {
		t.Errorf("古い足が無ければ何もしない: %v", got)
	}
}
