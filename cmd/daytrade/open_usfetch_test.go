package main

import (
	"testing"
	"time"
)

// 寄る前の回の米国市場の取得は、最悪の所要（budget + timeout）が「締め切り − usFetchReservePreopen」を
// 超えないこと（2026-09-21 のレビュー #9）。budget = 0 は「上限なし」なので、詰めた結果でも 0 にしない。
func TestUsFetchLimits(t *testing.T) {
	now := time.Date(2026, 9, 24, 8, 59, 52, 0, time.UTC)
	cases := []struct {
		name     string
		preopen  bool
		deadline time.Time
		timeout  time.Duration
		budget   time.Duration
	}{
		{"寄った後の回は従来どおり", false, now.Add(time.Second), usFetchTimeout, 0},
		{"締め切りなし（窓を見ない回）", true, time.Time{}, usFetchTimeoutPreopen, usFetchBudgetPreopen},
		{"余裕がある", true, now.Add(20 * time.Second), usFetchTimeoutPreopen, usFetchBudgetPreopen},
		{"残り 12 秒: ちょうど足りる", true, now.Add(12 * time.Second), usFetchTimeoutPreopen, usFetchBudgetPreopen},
		{"残り 8 秒: 取得に使えるのは 4 秒", true, now.Add(8 * time.Second), 3 * time.Second, time.Second},
		{"残り 6 秒: 取得に使えるのは 2 秒", true, now.Add(6 * time.Second), 2 * time.Second, time.Millisecond},
		{"残りが予約より短い: キャッシュだけ", true, now.Add(3 * time.Second), time.Millisecond, time.Millisecond},
		{"締め切りを過ぎている", true, now.Add(-time.Second), time.Millisecond, time.Millisecond},
	}
	for _, c := range cases {
		timeout, budget := usFetchLimits(c.preopen, c.deadline, now)
		if timeout != c.timeout || budget != c.budget {
			t.Errorf("%s: timeout=%s budget=%s, want %s / %s", c.name, timeout, budget, c.timeout, c.budget)
		}
		if room := c.deadline.Sub(now) - usFetchReservePreopen; c.preopen && !c.deadline.IsZero() && room > 0 && timeout+budget > room+time.Millisecond {
			t.Errorf("%s: 最悪の所要 %s が取得に使える %s を超える", c.name, timeout+budget, room)
		}
		if c.preopen && budget <= 0 {
			t.Errorf("%s: budget が 0（上限なし）になった", c.name)
		}
	}
}
