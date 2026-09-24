package main

import (
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
)

// 記録簿を使えない理由。取れていない日があればその日を名指しする（前日だけ失敗した朝を通さない）。
func TestCorpEventsStaleness(t *testing.T) {
	now := time.Date(2026, 9, 15, 9, 0, 0, 0, clock.Tokyo)
	cases := []struct {
		name string
		ev   corpEvents
		want string
	}{
		{"新しい", corpEvents{lastFetched: now.Add(-8 * time.Minute), freshDay: "2026-09-15"}, ""},
		{"前日が取れていない", corpEvents{freshDay: "2026-09-14"}, "2026-09-14 の取り込みに成功していない"},
		{"一度も無い", corpEvents{}, "取り込みに成功した日が 1 日も無い"},
		{"前日が夜の前まで", corpEvents{lastFetched: now.Add(-13 * time.Hour), freshDay: "2026-09-14"},
			"2026-09-14 ぶんの最後の取り込みが 09-14 20:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.ev.staleness(now, 90)
			if (c.want == "") != (got == "") || !strings.HasPrefix(got, c.want) {
				t.Errorf("staleness = %q, want %q", got, c.want)
			}
		})
	}
}
