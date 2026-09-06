package strategy

import (
	"math"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// levelBounceBars は「横ばい（75）→ 20 日で 100 まで上昇 → 5 日の押し目 → 反発日」の足。
//
// 上昇波は安値 74.25（横ばいの安値）→ 高値 100 なので、50% 押しは 87.125。
// pullback は押し目 5 日の終値、bounce は反発日 (open, high, low, close, volume)。
func levelBounceBars(pullback []float64, bounce [5]float64) []domain.Bar {
	const base = 5_000_000.0
	b := newBars("AAA").rise(230, 75, 0, base)
	prev := 75.0
	for i := 1; i <= 20; i++ {
		c := 75 + 25*float64(i)/20
		high := c + 0.2
		if i == 20 {
			high, c = 100, 99.8
		}
		b.add(prev, high, prev-0.2, c, base)
		prev = c
	}
	for _, c := range pullback {
		b.add(prev, prev+0.3, c-0.4, c, base)
		prev = c
	}
	b.add(bounce[0], bounce[1], bounce[2], bounce[3], bounce[4])
	return b.build()
}

var (
	goodPullback = []float64{97, 94, 91, 88.5, 87.5}      // 安値 87.0 ≒ 50% 押し
	goodBounce   = [5]float64{87.6, 91, 87.3, 90.5, 15e6} // 陽線・高値圏引け・出来高 3 倍
)

func levelBounceStrategy(t *testing.T, mutate func(*LevelBounceOptions)) *LevelBounce {
	t.Helper()
	o := DefaultLevelBounceOptions()
	if mutate != nil {
		mutate(&o)
	}
	s, err := NewLevelBounce(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func levelBounceContext(bars []domain.Bar, positions map[string]domain.Position) *Context {
	return NewContext("", map[string][]domain.Bar{"AAA": bars}, positions, decimal.Zero)
}

func TestLevelBounceEntersOnFibBounce(t *testing.T) {
	s := levelBounceStrategy(t, nil)
	ctx := levelBounceContext(levelBounceBars(goodPullback, goodBounce), nil)

	res := s.Screen(ctx, "AAA")
	if !res.Passed() {
		t.Fatalf("50%% 押しからの出来高を伴う反発は通るはず: %v (values %v)", res.Failed, res.Values)
	}
	if res.Setup != "fib_500" {
		t.Errorf("節目は fib_500 のはず: %s (level %.2f)", res.Setup, res.Values["level"])
	}
	if d := res.Values["depth"]; math.Abs(d-0.5) > 0.03 {
		t.Errorf("戻し比率が 50%% 付近のはず: %.3f", d)
	}
	if rr := res.Values["reward_risk"]; rr < 1.5 {
		t.Errorf("損益比が 1.5 以上のはず: %.2f", rr)
	}
	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction < directionFloor || sig.Direction > 1 {
		t.Errorf("direction は %.1f〜1.0: %f", directionFloor, sig.Direction)
	}
	if !strings.Contains(sig.Reason, "節目反発") {
		t.Errorf("理由に節目反発が要る: %s", sig.Reason)
	}
}

func TestLevelBounceRejectsWithoutVolume(t *testing.T) {
	s := levelBounceStrategy(t, nil)
	quiet := goodBounce
	quiet[4] = 5_000_000 // 平均並み → RVOL 1.0
	res := s.Screen(levelBounceContext(levelBounceBars(goodPullback, quiet), nil), "AAA")
	if res.Passed() || !hasReason(res.Failed, "RVOL") {
		t.Errorf("出来高を伴わない反発は落ちるはず: %v", res.Failed)
	}
}

func TestLevelBounceRejectsShallowPullback(t *testing.T) {
	s := levelBounceStrategy(t, nil)
	shallow := []float64{99, 98, 97.5, 97, 96.5} // 安値 96.1、38.2% 押し（90.2）に遠い
	bounce := [5]float64{96.6, 99.5, 96.3, 99, 15e6}
	res := s.Screen(levelBounceContext(levelBounceBars(shallow, bounce), nil), "AAA")
	if res.Passed() || !hasReason(res.Failed, "節目") {
		t.Errorf("節目に届かない押し目は落ちるはず: %v", res.Failed)
	}
}

func TestLevelBounceRejectsBearishBar(t *testing.T) {
	s := levelBounceStrategy(t, nil)
	bearish := [5]float64{87.6, 88, 86.9, 87.1, 15e6} // 節目に触れたが陰線
	res := s.Screen(levelBounceContext(levelBounceBars(goodPullback, bearish), nil), "AAA")
	if res.Passed() || !hasReason(res.Failed, "反転確認") {
		t.Errorf("陰線の日は落ちるはず: %v", res.Failed)
	}
}

func TestLevelBounceRejectsLateBounce(t *testing.T) {
	s := levelBounceStrategy(t, nil)
	// 押し目安値のあと 2 日戻してから見ると「反発したて」ではない
	late := append(append([]float64(nil), goodPullback...), 89, 90)
	res := s.Screen(levelBounceContext(levelBounceBars(late, [5]float64{90, 92, 89.8, 91.5, 15e6}), nil), "AAA")
	if res.Passed() || !hasReason(res.Failed, "反発したて") {
		t.Errorf("安値から日が経った反発は落ちるはず: %v", res.Failed)
	}
}

func TestLevelBounceRejectsDowntrend(t *testing.T) {
	s := levelBounceStrategy(t, func(o *LevelBounceOptions) { o.SMALong = 200 })
	// 横ばい部分を 120 に置くと SMA200 が終値 90 台より上になる
	b := newBars("AAA").rise(230, 120, 0, 5e6)
	prev := 120.0
	b.add(prev, 120.5, 74, 75, 5e6) // 急落してから上昇波
	prev = 75
	for i := 1; i <= 20; i++ {
		c := 75 + 25*float64(i)/20
		high := c + 0.2
		if i == 20 {
			high, c = 100, 99.8
		}
		b.add(prev, high, prev-0.2, c, 5e6)
		prev = c
	}
	for _, c := range goodPullback {
		b.add(prev, prev+0.3, c-0.4, c, 5e6)
		prev = c
	}
	b.add(goodBounce[0], goodBounce[1], goodBounce[2], goodBounce[3], goodBounce[4])
	res := s.Screen(levelBounceContext(b.build(), nil), "AAA")
	if res.Passed() || !hasReason(res.Failed, "トレンド") {
		t.Errorf("長期線の下では買わないはず: %v", res.Failed)
	}
}

func heldLevelBounce(t *testing.T, lastBar [5]float64) (domain.Signal, *LevelBounce) {
	t.Helper()
	s := levelBounceStrategy(t, nil)
	bars := levelBounceBars(goodPullback, goodBounce)
	b := newBars("AAA")
	for _, bar := range bars {
		o, _ := bar.Open.Float64()
		h, _ := bar.High.Float64()
		l, _ := bar.Low.Float64()
		c, _ := bar.Close.Float64()
		v, _ := bar.Volume.Float64()
		b.add(o, h, l, c, v)
	}
	b.add(lastBar[0], lastBar[1], lastBar[2], lastBar[3], lastBar[4])
	ctx := levelBounceContext(b.build(), map[string]domain.Position{"AAA": heldPosition("AAA", 100, 90.5)})
	return mustSignal(t, mustOnBars(t, s, ctx), "AAA"), s
}

func TestLevelBounceExitsOnTarget(t *testing.T) {
	sig, _ := heldLevelBounce(t, [5]float64{95, 101, 94.5, 100.5, 5e6})
	if sig.Direction != -1 || !strings.Contains(sig.Reason, "高値") {
		t.Errorf("前回高値到達で手仕舞うはず: %+v", sig)
	}
}

func TestLevelBounceExitsOnBreak(t *testing.T) {
	sig, _ := heldLevelBounce(t, [5]float64{88, 88.5, 84, 84.5, 5e6})
	if sig.Direction != -1 || !strings.Contains(sig.Reason, "押し目安値") {
		t.Errorf("押し目安値割れで手仕舞うはず: %+v", sig)
	}
}

func TestLevelBounceHoldsWhileWaiting(t *testing.T) {
	sig, _ := heldLevelBounce(t, [5]float64{90.5, 92, 90, 91.5, 5e6})
	if sig.Direction != 0.5 {
		t.Errorf("目標にも損切りにも届かなければ保有継続（0.5）のはず: %+v", sig)
	}
}

func TestRoundLevels(t *testing.T) {
	cases := []struct {
		lo, hi float64
		want   []float64
	}{
		{74.25, 100, nil},
		{1234, 1890, []float64{1500}},
		{523, 980, []float64{550, 600, 650, 700, 750, 800, 850, 900, 950}},
		{9000, 12000, []float64{10000}}, // 刻みは高値の桁（10,000 と 5,000 の倍数）。11,000 は入れない
	}
	for _, c := range cases {
		got := roundLevels(c.lo, c.hi)
		if len(got) != len(c.want) {
			t.Errorf("roundLevels(%g, %g) = %v, want %v", c.lo, c.hi, got, c.want)
			continue
		}
		for i := range got {
			if math.Abs(got[i]-c.want[i]) > 1e-9 {
				t.Errorf("roundLevels(%g, %g) = %v, want %v", c.lo, c.hi, got, c.want)
				break
			}
		}
	}
}

func TestLevelBounceRegistry(t *testing.T) {
	s, err := Create("level_bounce", map[string]any{"levels": []any{0.5, 0.618}, "round_numbers": false})
	if err != nil {
		t.Fatal(err)
	}
	lb := s.(*LevelBounce)
	if len(lb.Levels) != 2 || lb.Levels[1] != 0.618 || lb.RoundNumbers {
		t.Errorf("パラメータが反映されていない: %+v", lb)
	}
	if _, err := Create("level_bounce", map[string]any{"tolerance": 1.0}); err == nil {
		t.Error("知らないパラメータは弾くはず")
	}
}
