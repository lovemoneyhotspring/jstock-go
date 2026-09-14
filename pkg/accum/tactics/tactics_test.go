package tactics

import (
	"math"
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/window"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// barsFrom は 2026-09-01 から 1 日ずつ、終値だけを持つ足を作る。
func barsFrom(closes ...float64) []domain.Bar {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]domain.Bar, len(closes))
	for i, c := range closes {
		bars[i] = domain.Bar{Date: start.AddDate(0, 0, i).Format("2006-01-02"), Close: decimal.NewFromFloat(c)}
	}
	return bars
}

func assertMultipliers(t *testing.T, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("倍率の長さ = %d, want %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("倍率 = %v, want %v", got, want)
			return
		}
	}
}

// --- Base（発注時間帯） -------------------------------------------------

// 時間帯を設定し忘れたら既定の 14:00〜15:00 に倒す（「制限なし」にしない）。
func TestBaseWindowDefaultsToSafeWindow(t *testing.T) {
	c := &Constant{}
	if c.Window() != window.Default() {
		t.Fatalf("未設定の時間帯 = %+v, want 既定", c.Window())
	}

	at := func(day, hour, minute int) time.Time {
		return time.Date(2026, 9, day, hour, minute, 0, 0, clock.Tokyo)
	}
	cases := []struct {
		name   string
		moment time.Time
		want   bool
	}{
		{"開始ちょうどは含む", at(14, 14, 0), true},
		{"開始の 1 分前は外", at(14, 13, 59), false},
		{"終了の 1 分前は内", at(14, 14, 59), true},
		{"終了ちょうどは含まない", at(14, 15, 0), false},
		{"土曜は外", at(12, 14, 30), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.AllowsOrder(tc.moment); got != tc.want {
				t.Errorf("AllowsOrder = %v, want %v", got, tc.want)
			}
		})
	}

	c.SetWindow(window.Unrestricted())
	if !c.AllowsOrder(at(12, 3, 0)) {
		t.Error("制限なしに差し替えたら土曜の深夜でも許すべき")
	}
}

// --- Constant ----------------------------------------------------------

func TestConstant(t *testing.T) {
	c := &Constant{}
	if c.Name() != "constant" || c.Describe() != "constant" || c.WarmupBars() != 1 {
		t.Errorf("名前・説明・助走 = %s / %s / %d", c.Name(), c.Describe(), c.WarmupBars())
	}
	assertMultipliers(t, c.Multipliers(barsFrom(10, 5, 1)), []float64{1, 1, 1})
	assertMultipliers(t, c.Multipliers(nil), []float64{})
}

// --- BearStack ---------------------------------------------------------

func TestNewBearStack(t *testing.T) {
	cases := []struct {
		name            string
		mult            float64
		fast, mid, slow int
		wantErr         bool
		wantValue       float64
		wantPeriods     [3]int
	}{
		{"未指定は既定（×4, 20/50/200）", 0, 0, 0, 0, false, 4, [3]int{20, 50, 200}},
		{"負の倍率も既定", -1, 0, 0, 0, false, 4, [3]int{20, 50, 200}},
		{"倍率 1.0 ちょうどは許す", 1.0, 0, 0, 0, false, 1, [3]int{20, 50, 200}},
		{"指定を反映する", 2.5, 5, 10, 30, false, 2.5, [3]int{5, 10, 30}},
		{"1.0 未満は減額なので弾く", 0.99, 0, 0, 0, true, 0, [3]int{}},
		{"NaN は弾く", math.NaN(), 0, 0, 0, true, 0, [3]int{}},
		{"無限大は弾く", math.Inf(1), 0, 0, 0, true, 0, [3]int{}},
		{"中期と長期が同じなら弾く", 2, 20, 50, 50, true, 0, [3]int{}},
		{"既定と混ぜて順序が崩れたら弾く", 2, 0, 10, 0, true, 0, [3]int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBearStack(tc.mult, tc.fast, tc.mid, tc.slow)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("エラーにならない: %+v", b)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.Value != tc.wantValue || [3]int{b.Fast, b.Mid, b.Slow} != tc.wantPeriods {
				t.Errorf("= ×%g %d/%d/%d, want ×%g %v", b.Value, b.Fast, b.Mid, b.Slow, tc.wantValue, tc.wantPeriods)
			}
			if b.WarmupBars() != tc.wantPeriods[2] {
				t.Errorf("助走 = %d, want 長期 %d", b.WarmupBars(), tc.wantPeriods[2])
			}
		})
	}
}

func TestBearStackMultipliers(t *testing.T) {
	b, err := NewBearStack(4, 2, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "bear_stack" || b.Describe() != "bear_stack(×4, 2/3/4)" {
		t.Errorf("名前・説明 = %s / %s", b.Name(), b.Describe())
	}

	cases := []struct {
		name   string
		closes []float64
		want   []float64
	}{
		// i=3: 終値 7 < MA2 7.5 < MA3 8 < MA4 8.5
		{"完全下降配列で増額（長期線が揃う前は 1）", []float64{10, 9, 8, 7, 6}, []float64{1, 1, 1, 4, 4}},
		{"横ばいは等号なので増額しない", []float64{5, 5, 5, 5, 5}, []float64{1, 1, 1, 1, 1}},
		{"上昇は増額しない", []float64{1, 2, 3, 4, 5}, []float64{1, 1, 1, 1, 1}},
		{"足が長期より少なければすべて 1", []float64{10, 9, 8}, []float64{1, 1, 1}},
		{"足が無ければ空", nil, []float64{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertMultipliers(t, b.Multipliers(barsFrom(tc.closes...)), tc.want)
		})
	}
}

// --- StackLadder -------------------------------------------------------

func TestNewStackLadder(t *testing.T) {
	cases := []struct {
		name    string
		table   map[int]float64
		periods [3]int
		wantErr bool
	}{
		{"未指定は既定の段表", nil, [3]int{}, false},
		{"スコアの両端 0 と 6 は許す", map[int]float64{0: 1, 6: 2}, [3]int{}, false},
		{"同じ倍率が並ぶのは許す", map[int]float64{3: 2, 5: 2}, [3]int{}, false},
		{"スコア 7 は弾く", map[int]float64{7: 2}, [3]int{}, true},
		{"負のスコアは弾く", map[int]float64{-1: 2}, [3]int{}, true},
		{"1.0 未満の倍率は弾く", map[int]float64{3: 0.5}, [3]int{}, true},
		{"スコアが上がって倍率が下がるのは弾く", map[int]float64{3: 2, 5: 1.5}, [3]int{}, true},
		{"期間の順序が崩れたら弾く", nil, [3]int{50, 20, 200}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewStackLadder(tc.table, tc.periods[0], tc.periods[1], tc.periods[2])
			if tc.wantErr {
				if err == nil {
					t.Fatalf("エラーにならない: %+v", s)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.table == nil {
				if s.Describe() != "stack_ladder(3→×1.5, 5→×2, 6→×4)" {
					t.Errorf("既定の段表 = %s", s.Describe())
				}
				if s.Slow != 200 || s.WarmupBars() != 200 {
					t.Errorf("既定の長期 = %d / 助走 %d", s.Slow, s.WarmupBars())
				}
			}
		})
	}
}

func TestStackLadderMultipliers(t *testing.T) {
	cases := []struct {
		name   string
		table  map[int]float64
		closes []float64
		want   []float64
	}{
		// 6 条件すべて成立 → 6 点
		{"下降配列はスコア 6 で最上段", nil, []float64{10, 9, 8, 7}, []float64{1, 1, 1, 4}},
		// i=3: 終値 3 は MA2 6.5 / MA3 5 / MA4 4 のすべてより下だが、線の並びは上昇 → 3 点
		{"スコアが閾値ちょうどなら発動", map[int]float64{3: 1.5, 5: 2, 6: 4}, []float64{1, 2, 10, 3}, []float64{1, 1, 1, 1.5}},
		{"スコアが閾値に 1 足りなければ 1 倍", map[int]float64{4: 2}, []float64{1, 2, 10, 3}, []float64{1, 1, 1, 1}},
		{"横ばいはスコア 0", nil, []float64{5, 5, 5, 5}, []float64{1, 1, 1, 1}},
		{"スコア 0 の段があれば常に発動", map[int]float64{0: 1.2, 6: 3}, []float64{5, 5, 5, 5}, []float64{1, 1, 1, 1.2}},
		{"足が長期より少なければすべて 1", nil, []float64{10, 9, 8}, []float64{1, 1, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewStackLadder(tc.table, 2, 3, 4)
			if err != nil {
				t.Fatal(err)
			}
			assertMultipliers(t, s.Multipliers(barsFrom(tc.closes...)), tc.want)
		})
	}
}

// --- DrawdownLadder ----------------------------------------------------

func TestNewDrawdownLadder(t *testing.T) {
	cases := []struct {
		name     string
		levels   []float64
		values   []float64
		slow     int
		wantErr  bool
		wantSlow int
	}{
		{"未指定は既定の段表", nil, nil, 0, false, 200},
		{"下落率は 0 と 1 の内側なら許す", []float64{0.01, 0.99}, []float64{2, 3}, 50, false, 50},
		{"同じ倍率が並ぶのは許す", []float64{0.1, 0.2}, []float64{2, 2}, 0, false, 200},
		{"倍率が足りなければ弾く", []float64{0.1}, []float64{}, 0, true, 0},
		{"下落率が無く倍率だけなら弾く", []float64{}, []float64{2}, 0, true, 0},
		{"深い順に並んでいたら弾く", []float64{0.2, 0.1}, []float64{2, 3}, 0, true, 0},
		{"下落率 0 は弾く", []float64{0, 0.1}, []float64{2, 3}, 0, true, 0},
		{"下落率 1 は弾く", []float64{0.5, 1}, []float64{2, 3}, 0, true, 0},
		{"1.0 未満の倍率は弾く", []float64{0.1}, []float64{0.9}, 0, true, 0},
		{"深くなって倍率が下がるのは弾く", []float64{0.1, 0.2}, []float64{3, 2}, 0, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDrawdownLadder(tc.levels, tc.values, false, tc.slow)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("エラーにならない: %+v", d)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.Slow != tc.wantSlow {
				t.Errorf("長期 = %d, want %d", d.Slow, tc.wantSlow)
			}
		})
	}
}

func TestDrawdownLadderDescribeAndWarmup(t *testing.T) {
	d, err := NewDrawdownLadder(nil, nil, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "drawdown_ladder" || d.Describe() != "drawdown_ladder(-10%→×2, -20%→×3, -30%→×4)" {
		t.Errorf("名前・説明 = %s / %s", d.Name(), d.Describe())
	}
	if d.WarmupBars() != 1 {
		t.Errorf("下降の条件が無ければ助走は 1: %d", d.WarmupBars())
	}

	gated, err := NewDrawdownLadder([]float64{0.1}, []float64{2}, true, 30)
	if err != nil {
		t.Fatal(err)
	}
	if gated.Describe() != "drawdown_ladder(-10%→×2・200日線割れ時のみ)" {
		t.Errorf("説明 = %s", gated.Describe())
	}
	if gated.WarmupBars() != 30 {
		t.Errorf("下降の条件があれば助走は長期: %d", gated.WarmupBars())
	}
}

func TestDrawdownLadderMultipliers(t *testing.T) {
	cases := []struct {
		name        string
		requireDown bool
		closes      []float64
		want        []float64
	}{
		// 下落率: 0, -9%, -10%, -20%, -30%, 新高値 0, -10%
		{"段の境目ちょうどで発動し、新高値で基準が上がる", false,
			[]float64{100, 91, 90, 80, 70, 120, 108}, []float64{1, 1, 2, 3, 4, 1, 2}},
		{"下落が最深の段を超えても最上段のまま", false,
			[]float64{100, 10}, []float64{1, 4}},
		{"足が無ければ空", false, nil, []float64{}},
		// 長期 3: i<2 は線が無いので発動しない。i=2 は 80 < 86.7、i=3 は 60 < 73.3
		{"長期線が揃うまでは下落していても発動しない", true,
			[]float64{100, 80, 80, 60}, []float64{1, 1, 3, 4}},
		// i=3: 終値 50 = MA3 50 は「割れ」ではない
		{"終値が長期線と同じなら発動しない", true,
			[]float64{100, 50, 50, 50}, []float64{1, 1, 4, 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDrawdownLadder(nil, nil, tc.requireDown, 3)
			if err != nil {
				t.Fatal(err)
			}
			assertMultipliers(t, d.Multipliers(barsFrom(tc.closes...)), tc.want)
		})
	}
}
