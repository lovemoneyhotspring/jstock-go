package plan

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// fakeTactic は日付ごとに決めた倍率を返す（無い日は 1.0）。
type fakeTactic struct {
	tactics.Base
	byDate map[string]float64
}

func (f *fakeTactic) Name() string     { return "fake" }
func (f *fakeTactic) Describe() string { return "fake" }
func (f *fakeTactic) WarmupBars() int  { return 1 }
func (f *fakeTactic) Multipliers(bars []domain.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = 1.0
		if m, ok := f.byDate[b.Date]; ok {
			out[i] = m
		}
	}
	return out
}

func barsOn(dates ...string) []domain.Bar {
	bars := make([]domain.Bar, len(dates))
	for i, d := range dates {
		bars[i] = domain.Bar{Date: d, Close: decimal.NewFromInt(1000)}
	}
	return bars
}

func TestBuildPlanEmptyBars(t *testing.T) {
	p, err := BuildPlan(nil, &tactics.Constant{}, decimal.NewFromInt(100_000))
	if err != nil {
		t.Fatal(err)
	}
	if p == nil || len(p.Rows) != 0 {
		t.Errorf("足が無ければ計画は空: %+v", p)
	}
}

// 入金日は暦の 1 日ではなく、その月の最初の足。
func TestBuildPlanPaydayIsFirstBarOfEachMonth(t *testing.T) {
	bars := barsOn("2026-08-28", "2026-08-31", "2026-09-01", "2026-09-02")
	p, err := BuildPlan(bars, &tactics.Constant{}, decimal.NewFromInt(100_000))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		base   int64
		reason string
	}{
		{100_000, "入金日 100000 円"},
		{0, "投下なし"},
		{100_000, "入金日 100000 円"},
		{0, "投下なし"},
	}
	if len(p.Rows) != len(want) {
		t.Fatalf("行数 = %d, want %d", len(p.Rows), len(want))
	}
	for i, w := range want {
		r := p.Rows[i]
		if r.Date != bars[i].Date || !r.Close.Equal(bars[i].Close) || r.Multiplier != 1 {
			t.Errorf("行 %d の材料 = %+v", i, r)
		}
		if !r.Base.Equal(decimal.NewFromInt(w.base)) || !r.Amount.Equal(r.Base) || !r.Extra.IsZero() {
			t.Errorf("行 %d (%s) = base %s / extra %s / amount %s, want base %d", i, r.Date, r.Base, r.Extra, r.Amount, w.base)
		}
		if r.Reason != w.reason {
			t.Errorf("行 %d の理由 = %q, want %q", i, r.Reason, w.reason)
		}
	}
}

// 増額は日ごとに積み上げ、翌週の月曜以降の最初の足で予定に入れる。
// 積み上がりが月の予算に届いた日か、入金日にまとめて出す。
func TestBuildPlanReleasesAccruedExtra(t *testing.T) {
	// 9 月は 15 本（1 日あたりの増額 = (倍率 − 1) × 15000 / 15 = (倍率 − 1) × 1000）
	bars := barsOn(
		"2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11",
		"2026-09-14", "2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18",
		"2026-09-21", "2026-09-22", "2026-09-23", "2026-09-24", "2026-09-25",
		"2026-10-01",
	)
	budget := decimal.NewFromInt(15_000)

	cases := []struct {
		name       string
		mults      map[string]float64
		wantExtra  map[string]int64
		wantReason map[string]string
	}{
		{
			name:      "予算ちょうどに届いたら入金日を待たずに出す",
			mults:     map[string]float64{"2026-09-08": 16},
			wantExtra: map[string]int64{"2026-09-14": 15_000},
			wantReason: map[string]string{
				"2026-09-14": "累積の増額 15000 円（下降 1 日ぶん）",
				"2026-10-01": "入金日 15000 円",
			},
		},
		{
			name:      "予算に 1 円足りなければ次の入金日まで持ち越す",
			mults:     map[string]float64{"2026-09-08": 15.9995}, // floor(14999.5) = 14999
			wantExtra: map[string]int64{"2026-10-01": 14_999},
			wantReason: map[string]string{
				"2026-09-14": "投下なし",
				"2026-10-01": "入金日 15000 円＋累積の増額 14999 円（下降 1 日ぶん）",
			},
		},
		{
			name:      "倍率 1.0 ちょうどは増額しない",
			mults:     map[string]float64{"2026-09-08": 1.0, "2026-09-09": 1.0},
			wantExtra: map[string]int64{},
		},
		{
			name:      "月曜の増額は同じ日ではなく翌週の月曜に回す",
			mults:     map[string]float64{"2026-09-14": 16},
			wantExtra: map[string]int64{"2026-09-21": 15_000},
		},
		{
			name:      "週をまたいで積み上がり、予算に届いた週に出す",
			mults:     map[string]float64{"2026-09-08": 8.5, "2026-09-15": 8.5},
			wantExtra: map[string]int64{"2026-09-21": 15_000},
			wantReason: map[string]string{
				"2026-09-21": "累積の増額 15000 円（下降 2 日ぶん）",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := BuildPlan(bars, &fakeTactic{byDate: tc.mults}, budget)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Rows) != len(bars) {
				t.Fatalf("行数 = %d, want %d", len(p.Rows), len(bars))
			}
			for _, r := range p.Rows {
				want := decimal.NewFromInt(tc.wantExtra[r.Date])
				if !r.Extra.Equal(want) {
					t.Errorf("%s の増額 = %s, want %s", r.Date, r.Extra, want)
				}
				if !r.Amount.Equal(r.Base.Add(r.Extra)) {
					t.Errorf("%s の投下額 %s ≠ 基本 %s ＋ 増額 %s", r.Date, r.Amount, r.Base, r.Extra)
				}
				if reason, ok := tc.wantReason[r.Date]; ok && r.Reason != reason {
					t.Errorf("%s の理由 = %q, want %q", r.Date, r.Reason, reason)
				}
			}
		})
	}
}

// 判定用の足の倍率を、その日までに確定した判定用の足で引く。
// strict なら同じ日付の足は使わない（判定用の市場の引けが後で、判断時点に存在しない）。
func TestBuildPlanWithSignalAlignsByDate(t *testing.T) {
	bars := barsOn("2026-09-07", "2026-09-08", "2026-09-09")
	// 買う銘柄の日付（09-07）に倍率を置いても、判定用の足があれば使わない
	tactic := &fakeTactic{byDate: map[string]float64{"2026-09-04": 2, "2026-09-07": 5, "2026-09-08": 3}}

	cases := []struct {
		name   string
		signal []domain.Bar
		strict bool
		want   []float64
	}{
		{"同じ日付の判定用の足を使う", barsOn("2026-09-04", "2026-09-08"), false, []float64{2, 3, 3}},
		{"strict なら前日までの判定用の足だけ使う", barsOn("2026-09-04", "2026-09-08"), true, []float64{2, 2, 3}},
		{"判定用の足が始まる前は 1 倍", barsOn("2026-09-08"), false, []float64{1, 3, 3}},
		{"strict で判定用の足が始まる前は 1 倍", barsOn("2026-09-08"), true, []float64{1, 1, 3}},
		{"判定用の足が無ければ買う銘柄の足で判定", nil, false, []float64{5, 3, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := BuildPlanWithSignal(bars, tc.signal, tc.strict, tactic, decimal.NewFromInt(100_000))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Rows) != len(tc.want) {
				t.Fatalf("行数 = %d, want %d", len(p.Rows), len(tc.want))
			}
			for i, w := range tc.want {
				if p.Rows[i].Multiplier != w {
					t.Errorf("%s の倍率 = %g, want %g", p.Rows[i].Date, p.Rows[i].Multiplier, w)
				}
			}
		})
	}
}
