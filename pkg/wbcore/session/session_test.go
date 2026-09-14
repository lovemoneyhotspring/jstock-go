package session

import (
	"strings"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

func TestParseTime(t *testing.T) {
	cases := []struct {
		in           string
		hour, minute int
	}{
		{"09:30", 9, 30},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{" 9:05 ", 9, 5}, // 前後の空白と 1 桁の時は許す
	}
	for _, c := range cases {
		h, m, err := ParseTime(c.in, "entry_window")
		if err != nil || h != c.hour || m != c.minute {
			t.Errorf("ParseTime(%q) = %d:%d, %v; want %d:%d", c.in, h, m, err, c.hour, c.minute)
		}
	}
}

func TestParseTimeRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "9", "9:30:00", "24:00", "09:60", "-1:00", "ab:cd", "9-30"} {
		_, _, err := ParseTime(in, "exit_window")
		if err == nil {
			t.Errorf("ParseTime(%q) はエラーであるべき", in)
			continue
		}
		// どの設定項目が悪いかを示す
		if !strings.Contains(err.Error(), "exit_window") {
			t.Errorf("ParseTime(%q) のエラーに項目名が無い: %v", in, err)
		}
	}
}

func TestCloseUTC(t *testing.T) {
	day := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

	// 東証 15:30 JST = 06:30 UTC（夏時間は無い）
	jp, err := CloseUTC(domain.MarketJP, day)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 14, 6, 30, 0, 0, time.UTC); !jp.Equal(want) {
		t.Errorf("JP の引け = %v, want %v", jp, want)
	}

	// 米国 16:00 ET。夏時間（9 月）は 20:00 UTC、標準時（1 月）は 21:00 UTC
	us, err := CloseUTC(domain.MarketUS, day)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 14, 20, 0, 0, 0, time.UTC); !us.Equal(want) {
		t.Errorf("US の引け（夏時間）= %v, want %v", us, want)
	}
	winter := time.Date(2026, 1, 14, 0, 0, 0, 0, time.UTC)
	us, _ = CloseUTC(domain.MarketUS, winter)
	if want := time.Date(2026, 1, 14, 21, 0, 0, 0, time.UTC); !us.Equal(want) {
		t.Errorf("US の引け（標準時）= %v, want %v", us, want)
	}

	// ゼロ値は本日（UTC）
	now, err := CloseUTC(domain.MarketJP, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if today := time.Now().UTC(); now.Year() != today.Year() || now.YearDay() != today.YearDay() {
		t.Errorf("ゼロ値は今日: %v", now)
	}

	if _, err := CloseUTC(domain.Market("mars"), day); err == nil {
		t.Error("未定義の市場はエラー")
	}
}

func TestClosesAfter(t *testing.T) {
	day := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

	// 米国の引け（20:00 UTC）は東証（06:30 UTC）より後。同じ日付の指数の足は
	// 東証の判断時点にまだ無い
	after, err := ClosesAfter(domain.MarketUS, domain.MarketJP, day)
	if err != nil || !after {
		t.Errorf("US は JP より後に引ける: %v %v", after, err)
	}
	after, err = ClosesAfter(domain.MarketJP, domain.MarketUS, day)
	if err != nil || after {
		t.Errorf("JP は US より前に引ける: %v %v", after, err)
	}
	// 同じ市場は「後」ではない
	after, err = ClosesAfter(domain.MarketJP, domain.MarketJP, day)
	if err != nil || after {
		t.Errorf("同じ市場は偽: %v %v", after, err)
	}
	if _, err := ClosesAfter(domain.Market("mars"), domain.MarketJP, day); err == nil {
		t.Error("未定義の市場はエラー")
	}
}
