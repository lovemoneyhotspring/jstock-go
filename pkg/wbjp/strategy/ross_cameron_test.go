package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

const rcN = 120 // ウォームアップ（3×EMA20 + 5 + 2 = 67）を十分に超える本数

func rossStrategy(t *testing.T, mutate func(*RossCameronOptions)) *RossCameron {
	t.Helper()
	o := DefaultRossCameronOptions()
	o.Benchmark = "" // 地合いは別のテストで見る
	if mutate != nil {
		mutate(&o)
	}
	s, err := NewRossCameron(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// rossBase は EMA9 > EMA20 の上昇トレンドを作る土台。
func rossBase() *barBuilder {
	return newBars("AAA").rise(rcN-1, 100, 0.004, 1_000_000)
}

func rossContext(b *barBuilder, positions map[string]domain.Position) *Context {
	return NewContext("", map[string][]domain.Bar{"AAA": b.build()}, positions, decimal.Zero)
}

// A. Gap & Go: 材料日そのものに乗る。
func TestRossCameronGapAndGo(t *testing.T) {
	s := rossStrategy(t, nil)
	b := rossBase()
	prev := b.lastClose()
	// ギャップアップ・大商い・高値引け・直近高値のブレイク。
	open := prev * 1.06
	close := open * 1.03
	b.add(open, close*1.005, open*0.999, close, 4_000_000)

	ctx := rossContext(b, nil)
	res := s.Screen(ctx, "AAA")
	if !res.Passed() {
		t.Fatalf("Gap&Go は通るはず: %v", res.Failed)
	}
	if res.Setup != "Gap&Go" {
		t.Errorf("setup = %q", res.Setup)
	}
	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction < directionFloor {
		t.Errorf("direction = %g", sig.Direction)
	}
	if !contains(sig.Reason, "Gap&Go") {
		t.Errorf("reason = %s", sig.Reason)
	}
}

// ギャップが小さければ材料日にならない。
func TestRossCameronRejectsSmallGap(t *testing.T) {
	s := rossStrategy(t, func(o *RossCameronOptions) { o.AllowPullbackEntry = false })
	b := rossBase()
	prev := b.lastClose()
	open := prev * 1.001 // ギャップ不足
	close := open * 1.03
	b.add(open, close*1.005, open*0.999, close, 4_000_000)

	res := s.Screen(rossContext(b, nil), "AAA")
	if res.Passed() {
		t.Fatal("ギャップ不足は通してはいけない")
	}
	if !hasReason(res.Failed, "ギャップ") {
		t.Errorf("理由にギャップが要る: %v", res.Failed)
	}
}

// 出来高が伴わなければ「みんなが見ている株」ではない。
func TestRossCameronRejectsLowRVOL(t *testing.T) {
	s := rossStrategy(t, func(o *RossCameronOptions) { o.AllowPullbackEntry = false })
	b := rossBase()
	prev := b.lastClose()
	open := prev * 1.06
	close := open * 1.03
	b.add(open, close*1.005, open*0.999, close, 900_000) // 平均以下の出来高

	res := s.Screen(rossContext(b, nil), "AAA")
	if !hasReason(res.Failed, "RVOL") {
		t.Errorf("理由に RVOL が要る: %v", res.Failed)
	}
}

// B. マイクロプルバック: 材料日のあと押し目を挟んで再上昇に乗る。
func TestRossCameronMicroPullback(t *testing.T) {
	s := rossStrategy(t, func(o *RossCameronOptions) { o.AllowGapEntry = false })
	b := rossBase()

	// 材料日
	prev := b.lastClose()
	open := prev * 1.06
	catalystClose := open * 1.03
	b.add(open, catalystClose*1.005, open*0.999, catalystClose, 4_000_000)

	// 押し目 2 日（終値が前日を下回るが、安値は EMA9 を割らない）
	p1 := catalystClose * 0.99
	b.add(catalystClose, catalystClose*1.002, p1*0.998, p1, 1_500_000)
	p2 := p1 * 0.995
	b.add(p1, p1*1.002, p2*0.998, p2, 1_200_000)

	// 当日: 前日高値を陽線で抜け、出来高も前日超え
	prevHigh := p1 * 1.002
	todayClose := prevHigh * 1.03
	b.add(p2, todayClose*1.005, p2*0.998, todayClose, 3_000_000)

	ctx := rossContext(b, nil)
	res := s.Screen(ctx, "AAA")
	if !res.Passed() {
		t.Fatalf("マイクロプルバックは通るはず: %v", res.Failed)
	}
	if res.Setup != "マイクロプルバック" {
		t.Errorf("setup = %q", res.Setup)
	}
	// ギャップと RVOL は材料日の値を引き継ぐ。
	if res.Values["gap_pct"] < 0.03 {
		t.Errorf("材料日のギャップを引き継ぐはず: %g", res.Values["gap_pct"])
	}
}

// 材料日が無ければ押し目エントリーは成立しない。
func TestRossCameronPullbackNeedsCatalyst(t *testing.T) {
	s := rossStrategy(t, func(o *RossCameronOptions) { o.AllowGapEntry = false })
	b := rossBase()
	prev := b.lastClose()
	b.add(prev, prev*1.02, prev*0.99, prev*1.01, 1_000_000)

	res := s.Screen(rossContext(b, nil), "AAA")
	if res.Passed() {
		t.Fatal("材料日が無ければ通さない")
	}
	if !hasReason(res.Failed, "材料日") {
		t.Errorf("理由に材料日が要る: %v", res.Failed)
	}
}

// 本家の 9EMA トレーリング。EMA9 を割ったら降りる。
func TestRossCameronExitsBelowEMA(t *testing.T) {
	s := rossStrategy(t, nil)
	b := rossBase()
	prev := b.lastClose()
	crashed := prev * 0.80 // EMA9 を大きく割る
	b.add(prev, prev*1.001, crashed*0.99, crashed, 2_000_000)

	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 100)}
	sig := mustSignal(t, mustOnBars(t, s, rossContext(b, positions)), "AAA")
	if sig.Direction != -1.0 || !contains(sig.Reason, "EMA9") {
		t.Errorf("EMA9 割れで手仕舞うはず: %+v", sig)
	}
}

// EMA9 の上にいる限りは保有継続（弱い買い）。
func TestRossCameronHoldsAboveEMA(t *testing.T) {
	s := rossStrategy(t, nil)
	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 100)}
	sig := mustSignal(t, mustOnBars(t, s, rossContext(rossBase(), positions)), "AAA")
	if sig.Direction != 0.5 || !contains(sig.Reason, "保有継続") {
		t.Errorf("保有継続は弱い買い: %+v", sig)
	}
}

// 地合いフィルタ（ベンチマークは既定で有効）。
func TestRossCameronBlockedByWeakBenchmark(t *testing.T) {
	o := DefaultRossCameronOptions()
	s, err := NewRossCameron(o)
	if err != nil {
		t.Fatal(err)
	}
	b := rossBase()
	prev := b.lastClose()
	open := prev * 1.06
	close := open * 1.03
	b.add(open, close*1.005, open*0.999, close, 4_000_000)

	bars := map[string][]domain.Bar{
		"AAA": b.build(),
		"SPY": barsFrom("SPY", growth(-0.004, rcN, 300, 0.005), 5_000_000, nil),
	}
	res := s.Screen(NewContext("", bars, nil, decimal.Zero), "AAA")
	if res.Passed() {
		t.Fatal("地合いオフでは通してはいけない")
	}
	if !hasReason(res.Failed, "地合い") {
		t.Errorf("理由に地合いが要る: %v", res.Failed)
	}
}

func TestRossCameronRejectsBadOptions(t *testing.T) {
	o := DefaultRossCameronOptions()
	o.AllowGapEntry, o.AllowPullbackEntry = false, false
	if _, err := NewRossCameron(o); err == nil {
		t.Error("入口を両方切る設定は弾くべき")
	}

	o = DefaultRossCameronOptions()
	o.MaxPullbackDays, o.CatalystLookback = 5, 3
	if _, err := NewRossCameron(o); err == nil {
		t.Error("max_pullback_days >= catalyst_lookback は弾くべき")
	}

	o = DefaultRossCameronOptions()
	o.EMAFast, o.EMASlow = 20, 9
	if _, err := NewRossCameron(o); err == nil {
		t.Error("ema_fast >= ema_slow は弾くべき")
	}
}
