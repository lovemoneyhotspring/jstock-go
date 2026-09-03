package strategy

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestATRBreakoutUpward(t *testing.T) {
	s, err := NewATRBreakout(20, 14, 0.005)
	if err != nil {
		t.Fatal(err)
	}
	b := newBars("AAA")
	for i := 0; i < 40; i++ {
		b.add(100, 102, 98, 100, 1000)
	}
	b.add(101, 130, 100, 128, 3000) // 20日高値を大きく上抜け
	ctx := NewContext("", map[string][]domain.Bar{"AAA": b.build()}, nil, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != 1.0 {
		t.Errorf("上抜けは買い: %+v", sig)
	}
}

func TestATRBreakoutDownward(t *testing.T) {
	s, _ := NewATRBreakout(20, 14, 0.005)
	b := newBars("AAA")
	for i := 0; i < 40; i++ {
		b.add(100, 102, 98, 100, 1000)
	}
	b.add(99, 100, 70, 72, 3000)
	ctx := NewContext("", map[string][]domain.Bar{"AAA": b.build()}, nil, decimal.Zero)

	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if sig.Direction != -1.0 {
		t.Errorf("下抜けは売り: %+v", sig)
	}
}

// 値動きが乏しい局面はだましになりやすいので見送る。
func TestATRBreakoutSkipsLowVolatility(t *testing.T) {
	s, _ := NewATRBreakout(20, 14, 0.05)
	b := newBars("AAA")
	for i := 0; i < 40; i++ {
		b.add(100, 100.1, 99.9, 100, 1000)
	}
	b.add(100, 101, 100, 100.5, 1000)
	ctx := NewContext("", map[string][]domain.Bar{"AAA": b.build()}, nil, decimal.Zero)

	if sigs := mustOnBars(t, s, ctx); len(sigs) != 0 {
		t.Errorf("ATR 比が下限未満なら見送る: %+v", sigs)
	}
}

// ドンチャンの高値は当日を除く。当日を含めると必ずブレイクが成立し、
// 「必ず勝つ」嘘のバックテスト結果になる。
func TestATRBreakoutExcludesTodayFromChannel(t *testing.T) {
	s, _ := NewATRBreakout(20, 14, 0.005)
	b := newBars("AAA")
	for i := 0; i < 40; i++ {
		b.add(100, 104, 96, 100, 1000)
	}
	// 当日の高値は過去の高値（104）を超えない。ブレイクにはならないはず。
	b.add(100, 103, 97, 100, 1000)
	ctx := NewContext("", map[string][]domain.Bar{"AAA": b.build()}, nil, decimal.Zero)

	if sigs := mustOnBars(t, s, ctx); len(sigs) != 0 {
		t.Errorf("当日高値が過去高値以下ならブレイクではない: %+v", sigs)
	}
}

func TestATRBreakoutMeta(t *testing.T) {
	s, _ := NewATRBreakout(20, 14, 0.005)
	b := newBars("AAA")
	for i := 0; i < 40; i++ {
		b.add(100, 102, 98, 100, 1000)
	}
	b.add(101, 130, 100, 128, 3000)
	ctx := NewContext("", map[string][]domain.Bar{"AAA": b.build()}, nil, decimal.Zero)
	sig := mustSignal(t, mustOnBars(t, s, ctx), "AAA")
	if _, ok := sig.Meta["atr_ratio"]; !ok {
		t.Errorf("meta に atr_ratio が要る: %v", sig.Meta)
	}
}

var _ = domain.Bar{}
