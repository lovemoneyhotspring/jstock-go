package backtest_test

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// addMinutes は最後の 10 日（ギャップを入れた日）に分足を書く。
// 10000 は 09:00 に寄り、50000 は特別気配で 09:06 まで寄らない。
func addMinutes(t *testing.T, arch *archive.Archive, days []time.Time) {
	t.Helper()
	for _, day := range days[len(days)-10:] {
		if err := fixture.AddMinuteBars(arch, day, map[string][]fixture.MinuteBar{
			// 09:00 に寄る銘柄。9:01 は寄付より高い（寄り後の反発）
			"10000": {
				{Time: "09:00", Price: 970}, {Time: "09:01", Price: 980},
				{Time: "15:20", Price: 990}, {Time: "15:30", Price: 989},
			},
			// 特別気配で 09:06 まで寄らない銘柄
			"20000": {{Time: "09:06", Price: 2160}, {Time: "15:20", Price: 2100}},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMinuteBarsFillAndOpened(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	addMinutes(t, arch, days)
	last := days[len(days)-1]

	minute, err := backtest.LoadMinuteBars(arch, days[len(days)-10], last, "09:01", "15:20", nil)
	if err != nil {
		t.Fatal(err)
	}
	if minute.Days() != 10 {
		t.Fatalf("分足のある日 = %d, want 10", minute.Days())
	}

	// 分足のある日: 建値は 09:01 以降の最初の約定、手仕舞いは 15:20 以降の最初の約定
	row := backtest.Row{Date: last, Code: "10000", Open: 970, Close: 989}
	entry, exit, ok := minute.Fill(row)
	if !ok || entry != 980 || exit != 990 {
		t.Errorf("Fill = (%v, %v, %v), want (980, 990, true)", entry, exit, ok)
	}
	// 分足の無い日は日足の寄付・引けに落ちる（10 年の検証を止めないため）
	early := backtest.Row{Date: days[0], Code: "10000", Open: 1000, Close: 1010}
	entry, exit, ok = minute.Fill(early)
	if !ok || entry != 1000 || exit != 1010 {
		t.Errorf("分足の無い日の Fill = (%v, %v, %v), want (1000, 1010, true)", entry, exit, ok)
	}
	// 分足のある日に足が 1 本も無い銘柄は建てられない
	if _, _, ok := minute.Fill(backtest.Row{Date: last, Code: "30000", Open: 1500, Close: 1500}); ok {
		t.Error("その日 1 度も約定していない銘柄で建てている")
	}

	if opened, known := minute.Opened(last, "10000"); !known || !opened {
		t.Errorf("Opened(10000) = (%v, %v), want (true, true)", opened, known)
	}
	if opened, known := minute.Opened(last, "20000"); !known || opened {
		t.Errorf("Opened(20000) = (%v, %v), want (false, true)", opened, known)
	}
	if _, known := minute.Opened(days[0], "10000"); known {
		t.Error("分足の無い日を判定している")
	}
}

func TestSimulateSkipOpened(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	addMinutes(t, arch, days)
	from, to := days[30], days[len(days)-1]

	cfg := baseConfig()
	panel, err := backtest.LoadPanel(arch, from, to, cfg)
	if err != nil {
		t.Fatal(err)
	}
	minute, err := backtest.LoadMinuteBars(arch, from, to, "", "", backtest.PanelCodes(panel))
	if err != nil {
		t.Fatal(err)
	}

	// 既定（skip_opened = false）は 09:00 に寄った 10000 を建てる
	base, err := backtest.SimulateWith(panel, cfg, nil, backtest.Options{Opened: minute.Opened})
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Trades) == 0 {
		t.Fatal("取引が 1 件も無い")
	}

	// skip_opened = true にすると、その銘柄は候補から消える
	cfg.Signal.SkipOpened = true
	filtered, err := backtest.SimulateWith(panel, cfg, nil, backtest.Options{Opened: minute.Opened})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range filtered.Trades {
		if tr.Code == "10000" {
			t.Errorf("09:00 に寄った銘柄を建てている: %+v", tr)
		}
	}
	if len(filtered.Trades) >= len(base.Trades) {
		t.Errorf("取引数 %d, want < %d（除外が効いていない）", len(filtered.Trades), len(base.Trades))
	}

	// 分足を渡さなければ設定が真でも絞らない（分足の無い期間で挙動を変えないため）
	unfiltered, err := backtest.SimulateWith(panel, cfg, nil, backtest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered.Trades) != len(base.Trades) {
		t.Errorf("分足なしの取引数 %d, want %d", len(unfiltered.Trades), len(base.Trades))
	}
}

func TestLoadMinuteBarsRejectsBadTime(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	addMinutes(t, arch, days)
	if _, err := backtest.LoadMinuteBars(arch, days[len(days)-10], days[len(days)-1], "9時", "", nil); err == nil {
		t.Error("時刻の書式が不正なのにエラーにならない")
	}
}
