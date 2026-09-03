package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

const momN = 300

// momBars は既定の母集団。SPY は緩やかな上昇、STRONG > MILD > SPY、WEAK は下落。
// 最終日は月初なので月次入れ替えの日にあたる。
func momBars(dates []string) map[string][]domain.Bar {
	if dates == nil {
		dates = monthStartDates(momN)
	}
	rates := map[string]float64{
		"SPY":    0.0008,
		"STRONG": 0.003,
		"MILD":   0.0015,
		"WEAK":   -0.001,
	}
	out := make(map[string][]domain.Bar, len(rates))
	for sym, r := range rates {
		out[sym] = barsFrom(sym, growth(r, momN, 100, 0.01), 3_000_000, dates)
	}
	return out
}

func momStrategy(t *testing.T, mutate func(*MomentumRankOptions)) *MomentumRank {
	t.Helper()
	o := DefaultMomentumRankOptions()
	if mutate != nil {
		mutate(&o)
	}
	m, err := NewMomentumRank(o)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 銘柄横断の順位付け。これは 1 銘柄ずつ評価する形では原理的に書けない。
func TestMomentumRankOrdersAcrossSymbols(t *testing.T) {
	m := momStrategy(t, nil)
	ctx := NewContext("", momBars(nil), nil, decimal.Zero)

	sigs := mustOnBars(t, m, ctx)
	strong := mustSignal(t, sigs, "STRONG")
	mild := mustSignal(t, sigs, "MILD")

	// 上位ほど direction が大きい。サイジングはこの順に枠を埋める。
	if strong.Direction <= mild.Direction {
		t.Errorf("STRONG(%g) は MILD(%g) より上位のはず", strong.Direction, mild.Direction)
	}
	if rank, _ := strong.Meta["rank"].(int); rank != 1 {
		t.Errorf("STRONG の順位 = %v, 期待 1", strong.Meta["rank"])
	}
	// ベンチマーク自身は売買対象にしない。
	if _, ok := signalFor(t, sigs, "SPY"); ok {
		t.Error("ベンチマークにシグナルを出してはいけない")
	}
	// 下落銘柄は母集団に残らない。
	if _, ok := signalFor(t, sigs, "WEAK"); ok {
		t.Error("下落銘柄は候補に入れない")
	}
}

// 地合いフィルタ。ベンチマークが SMA を割ったら新規建てを止める。
// これも銘柄横断の材料なので、旧インターフェースでは実装できなかった。
func TestMomentumRankStopsWhenBenchmarkIsWeak(t *testing.T) {
	m := momStrategy(t, nil)
	bars := momBars(nil)
	// SPY を長期線割れの下落基調に差し替える。
	bars["SPY"] = barsFrom("SPY", growth(-0.002, momN, 100, 0.01), 3_000_000, monthStartDates(momN))
	ctx := NewContext("", bars, nil, decimal.Zero)

	for _, sig := range mustOnBars(t, m, ctx) {
		if sig.Direction > 0 {
			t.Errorf("地合いオフでは新規建てしない: %+v", sig)
		}
	}
}

// ベンチマークの足が無いときは「判断できない」ので建てない（黙って通さない）。
func TestMomentumRankRefusesWithoutBenchmark(t *testing.T) {
	m := momStrategy(t, nil)
	bars := momBars(nil)
	delete(bars, "SPY")
	ctx := NewContext("", bars, nil, decimal.Zero)

	for _, sig := range mustOnBars(t, m, ctx) {
		if sig.Direction > 0 {
			t.Errorf("ベンチマーク不在では建てない: %+v", sig)
		}
	}
}

// 新規建ては入れ替えの区切りの日だけ。毎日入れ替えると売買代金がかさむ。
func TestMomentumRankEntersOnlyOnRebalanceDay(t *testing.T) {
	m := momStrategy(t, nil)
	// 最終日が月初でない日付列にする。
	dates := weekdays(momN, mustDate("2025-01-15"))
	ctx := NewContext("", momBars(dates), nil, decimal.Zero)

	for _, sig := range mustOnBars(t, m, ctx) {
		if sig.Direction > 0 {
			t.Errorf("区切りの日以外は新規建てしない: %+v", sig)
		}
	}

	daily := momStrategy(t, func(o *MomentumRankOptions) { o.Rebalance = "daily" })
	if sigs := mustOnBars(t, daily, ctx); len(sigs) == 0 {
		t.Error("rebalance=daily なら毎日建てられる")
	}
}

// 保有中にトレンドが崩れたら、区切りの日を待たず毎日判定して降りる。
func TestMomentumRankExitsOnTrendBreak(t *testing.T) {
	m := momStrategy(t, nil)
	bars := momBars(nil)
	// FALLEN は上げたあとに急落させ、SMA100 を割らせる。
	closes := growth(0.003, momN-40, 100, 0.01)
	fallen := closes[len(closes)-1]
	for i := 0; i < 40; i++ {
		fallen *= 0.97
		closes = append(closes, fallen)
	}
	bars["FALLEN"] = barsFrom("FALLEN", closes, 3_000_000, monthStartDates(momN))

	positions := map[string]domain.Position{"FALLEN": heldPosition("FALLEN", 100, 100)}
	ctx := NewContext("", bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, m, ctx), "FALLEN")
	if sig.Direction != -1.0 || !contains(sig.Reason, "トレンド崩れ") {
		t.Errorf("トレンド崩れで手仕舞うはず: %+v", sig)
	}
}

// 保有継続は「意見なし」ではなく弱い買いを返す。意見が消えると
// サイジングが手仕舞い扱いにしてしまう。
func TestMomentumRankHoldsWithWeakBuy(t *testing.T) {
	m := momStrategy(t, nil)
	positions := map[string]domain.Position{"STRONG": heldPosition("STRONG", 100, 100)}
	ctx := NewContext("", momBars(nil), positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, m, ctx), "STRONG")
	if sig.Direction != 0.5 || !contains(sig.Reason, "保有継続") {
		t.Errorf("保有継続は弱い買い: %+v", sig)
	}
}

// 候補が枠に満たないときは受け皿（通常はベンチマーク）で埋める。
// 現金が遊んで指数に劣後するのを防ぐ。
func TestMomentumRankFallsBackToCoreSymbol(t *testing.T) {
	m := momStrategy(t, func(o *MomentumRankOptions) {
		o.TopN = 5
		o.CoreSymbol = "CORE"
	})
	bars := momBars(nil)
	bars["CORE"] = barsFrom("CORE", growth(0.001, momN, 100, 0.01), 3_000_000, monthStartDates(momN))
	ctx := NewContext("", bars, nil, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, m, ctx), "CORE")
	if sig.Direction != directionFloor {
		t.Errorf("受け皿は最下位扱い: direction=%g", sig.Direction)
	}
	if !contains(sig.Reason, "受け皿") {
		t.Errorf("reason = %s", sig.Reason)
	}
}

// 受け皿は本命が枠を埋められるようになったら降りる。
func TestMomentumRankReleasesCoreWhenCandidatesFill(t *testing.T) {
	m := momStrategy(t, func(o *MomentumRankOptions) {
		o.TopN = 1
		o.CoreSymbol = "CORE"
	})
	bars := momBars(nil)
	bars["CORE"] = barsFrom("CORE", growth(0.001, momN, 100, 0.01), 3_000_000, monthStartDates(momN))
	positions := map[string]domain.Position{"CORE": heldPosition("CORE", 100, 100)}
	ctx := NewContext("", bars, positions, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, m, ctx), "CORE")
	if sig.Direction != -1.0 || !contains(sig.Reason, "受け皿を解除") {
		t.Errorf("候補が枠を埋められるなら受け皿は降りる: %+v", sig)
	}
}

// screen は落選理由を並べられる（--show-failed の土台）。
func TestMomentumRankScreenExplainsFailure(t *testing.T) {
	m := momStrategy(t, nil)
	ctx := NewContext("", momBars(nil), nil, decimal.Zero)

	res := m.Screen(ctx, "WEAK")
	if res.Passed() {
		t.Fatal("WEAK は落ちるはず")
	}
	if !hasReason(res.Failed, "リターン") {
		t.Errorf("理由にリターンの記述が要る: %v", res.Failed)
	}

	if !m.Screen(ctx, "STRONG").Passed() {
		t.Errorf("STRONG は通るはず: %v", m.Screen(ctx, "STRONG").Failed)
	}
}

func TestMomentumRankRejectsBadLookbacks(t *testing.T) {
	o := DefaultMomentumRankOptions()
	o.LongLookback = 50 // lookback + skip より小さい
	if _, err := NewMomentumRank(o); err == nil {
		t.Error("long_lookback <= lookback + skip は弾くべき")
	}
}
