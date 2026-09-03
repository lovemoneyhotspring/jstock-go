package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

const pbN = 300

// pullbackUniverse は長期上昇トレンドの銘柄と、上昇中のベンチマーク。
func pullbackUniverse(symbolBars []domain.Bar) map[string][]domain.Bar {
	return map[string][]domain.Bar{
		"AAA": symbolBars,
		"SPY": barsFrom("SPY", growth(0.0005, pbN, 100, 0.005), 5_000_000, nil),
	}
}

// weakBenchmark は長期線を割ったベンチマーク。
func weakBenchmark() []domain.Bar {
	return barsFrom("SPY", growth(-0.003, pbN, 300, 0.005), 5_000_000, nil)
}

// uptrendWithDip は長期上昇のあと数日下げ、最終日に反発する終値列。
func uptrendWithDip() []float64 {
	closes := growth(0.004, pbN-6, 50, 0.004)
	p := closes[len(closes)-1]
	for i := 0; i < 5; i++ { // 押し目（連続陰線）
		p *= 0.975
		closes = append(closes, p)
	}
	closes = append(closes, p*1.03) // 反発（陽線）
	return closes
}

func rsiPullbackStrategy(t *testing.T, mutate func(*RSIPullbackOptions)) *RSIPullback {
	t.Helper()
	o := DefaultRSIPullbackOptions()
	if mutate != nil {
		mutate(&o)
	}
	s, err := NewRSIPullback(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRSIPullbackEntersOnDip(t *testing.T) {
	s := rsiPullbackStrategy(t, nil)
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	ctx := NewContext("", bars, nil, decimal.Zero)

	res := s.Screen(ctx, "AAA")
	if !res.Passed() {
		t.Fatalf("押し目からの反発は通るはず: %v", res.Failed)
	}
	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction < directionFloor {
		t.Errorf("direction は下限以上のはず: %g", sig.Direction)
	}
}

// 地合いフィルタ。旧インターフェースでは他銘柄を見られず実装できなかった。
func TestRSIPullbackBlockedByWeakBenchmark(t *testing.T) {
	s := rsiPullbackStrategy(t, nil)
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	bars["SPY"] = weakBenchmark()
	ctx := NewContext("", bars, nil, decimal.Zero)

	res := s.Screen(ctx, "AAA")
	if res.Passed() {
		t.Fatal("地合いオフでは通してはいけない")
	}
	if !hasReason(res.Failed, "地合い") {
		t.Errorf("理由に地合いが要る: %v", res.Failed)
	}
	if _, ok := signalFor(t, mustOnBars(t, s, ctx), "AAA"); ok {
		t.Error("地合いオフで新規シグナルを出してはいけない")
	}
}

// ベンチマークの足が無ければ判断できない。黙って通すより止める。
func TestRSIPullbackBlockedByMissingBenchmark(t *testing.T) {
	s := rsiPullbackStrategy(t, nil)
	bars := map[string][]domain.Bar{"AAA": barsFrom("AAA", uptrendWithDip(), 5_000_000, nil)}
	ctx := NewContext("", bars, nil, decimal.Zero)

	if s.Screen(ctx, "AAA").Passed() {
		t.Error("ベンチマーク不在では通さない")
	}
}

func TestRSIPullbackExitsOnOverbought(t *testing.T) {
	s := rsiPullbackStrategy(t, func(o *RSIPullbackOptions) {
		o.ExitOnHigh = false
		o.ExitOnSMARecovery = false
	})
	// 直近が連騰していれば RSI(3) は買われすぎ圏に張り付く。
	bars := pullbackUniverse(barsFrom("AAA", growth(0.004, pbN, 50, 0.004), 5_000_000, nil))
	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 1e9)} // 含み損にして SMA 回復条件を避ける
	ctx := NewContext("", bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != -1.0 || !contains(sig.Reason, "買われすぎ") {
		t.Errorf("買われすぎで手仕舞うはず: %+v", sig)
	}
}

// 出口はそれぞれ個別に無効化できる（出口の検証用）。
func TestRSIPullbackExitsCanBeDisabled(t *testing.T) {
	s := rsiPullbackStrategy(t, func(o *RSIPullbackOptions) {
		o.ExitOnRSI = false
		o.ExitOnHigh = false
		o.ExitOnSMARecovery = false
	})
	bars := pullbackUniverse(barsFrom("AAA", growth(0.004, pbN, 50, 0.004), 5_000_000, nil))
	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 1e9)}
	ctx := NewContext("", bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != 0.5 {
		t.Errorf("出口を全部切れば保有継続: %+v", sig)
	}
}

func TestRSIPullbackRejectsBadThresholds(t *testing.T) {
	o := DefaultRSIPullbackOptions()
	o.RSIEntry, o.RSIExit = 80, 20
	if _, err := NewRSIPullback(o); err == nil {
		t.Error("rsi_entry < rsi_exit を満たさない設定は弾くべき")
	}
}

// -- trend_pullback --------------------------------------------------------

func trendPullbackStrategy(t *testing.T, mutate func(*TrendPullbackOptions)) *TrendPullback {
	t.Helper()
	o := DefaultTrendPullbackOptions()
	if mutate != nil {
		mutate(&o)
	}
	s, err := NewTrendPullback(o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTrendPullbackBlockedByWeakBenchmark(t *testing.T) {
	s := trendPullbackStrategy(t, nil)
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	bars["SPY"] = weakBenchmark()
	ctx := NewContext("", bars, nil, decimal.Zero)

	res := s.Screen(ctx, "AAA")
	if res.Passed() {
		t.Fatal("地合いオフでは通してはいけない")
	}
	if !hasReason(res.Failed, "地合い") {
		t.Errorf("理由に地合いが要る: %v", res.Failed)
	}
}

// 相対的強さ（RS）はベンチマークとの比較なので、銘柄横断の材料。
func TestTrendPullbackRequiresRelativeStrength(t *testing.T) {
	s := trendPullbackStrategy(t, nil)
	// ベンチマークのほうが強い局面を作る。
	bars := pullbackUniverse(barsFrom("AAA", growth(0.0005, pbN, 100, 0.004), 5_000_000, nil))
	bars["SPY"] = barsFrom("SPY", growth(0.004, pbN, 100, 0.004), 5_000_000, nil)
	ctx := NewContext("", bars, nil, decimal.Zero)

	res := s.Screen(ctx, "AAA")
	if res.Passed() {
		t.Fatal("ベンチマークに負けている銘柄は通さない")
	}
	if !hasReason(res.Failed, "RS") {
		t.Errorf("理由に RS が要る: %v", res.Failed)
	}
}

// 保有中の手仕舞いは決算だけ。損切り・利確はストップ管理に委ねる。
func TestTrendPullbackHoldsWithWeakBuy(t *testing.T) {
	s := trendPullbackStrategy(t, nil)
	bars := pullbackUniverse(barsFrom("AAA", uptrendWithDip(), 5_000_000, nil))
	positions := map[string]domain.Position{"AAA": heldPosition("AAA", 100, 100)}
	ctx := NewContext("", bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != 0.5 || !contains(sig.Reason, "保有継続") {
		t.Errorf("保有継続は弱い買い: %+v", sig)
	}
}

func TestTrendPullbackRejectsBadSMAOrder(t *testing.T) {
	o := DefaultTrendPullbackOptions()
	o.SMAShort, o.SMALong = 200, 20
	if _, err := NewTrendPullback(o); err == nil {
		t.Error("sma_short < sma_mid < sma_long を満たさない設定は弾くべき")
	}
}
