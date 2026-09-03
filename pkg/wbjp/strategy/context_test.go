package strategy

import (
	"math"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/shopspring/decimal"
)

func TestContextTruncatesAtAsOf(t *testing.T) {
	bars := barsFrom("AAA", growth(0.001, 50, 100, 0), 1000, nil)
	u := NewUniverse(map[string][]domain.Bar{"AAA": bars})

	cutoff := bars[29].Date
	ctx := u.At(cutoff, nil, decimal.Zero)
	v, ok := ctx.Bars("AAA")
	if !ok {
		t.Fatal("足が見えない")
	}
	// 未来の足が見える手段があってはならない。
	if v.Len() != 30 {
		t.Fatalf("見える本数 = %d, 期待 30", v.Len())
	}
	if v.LastDate() != cutoff {
		t.Fatalf("最終日 = %s, 期待 %s", v.LastDate(), cutoff)
	}
}

// 全履歴で計算してから切り詰めた指標が、切り詰めてから計算したものと一致すること。
// これが崩れると、キャッシュのために先読みが混入する。
func TestIndicatorCacheMatchesTruncatedComputation(t *testing.T) {
	closes := growth(0.002, 120, 100, 0.03)
	bars := barsFrom("AAA", closes, 1000, nil)
	u := NewUniverse(map[string][]domain.Bar{"AAA": bars})

	cut := 80
	ctx := u.At(bars[cut-1].Date, nil, decimal.Zero)
	v, _ := ctx.Bars("AAA")

	want, err := indicators.SMA(closes[:cut], 20)
	if err != nil {
		t.Fatal(err)
	}
	got := v.SMA(20)
	if len(got) != len(want) {
		t.Fatalf("長さ = %d, 期待 %d", len(got), len(want))
	}
	for i := range want {
		if math.IsNaN(want[i]) != math.IsNaN(got[i]) {
			t.Fatalf("i=%d: NaN の位置が違う (%v vs %v)", i, got[i], want[i])
		}
		if !math.IsNaN(want[i]) && math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("i=%d: %g != %g", i, got[i], want[i])
		}
	}
}

func TestContextPositions(t *testing.T) {
	bars := barsFrom("AAA", growth(0.001, 10, 100, 0), 1000, nil)
	positions := map[string]domain.Position{
		"AAA": heldPosition("AAA", 100, 90),
		"BBB": {Symbol: "BBB", Quantity: decimal.Zero},
	}
	ctx := NewContext("", map[string][]domain.Bar{"AAA": bars}, positions, decimal.NewFromInt(100))

	if !ctx.HasPosition("AAA") {
		t.Error("AAA は保有中のはず")
	}
	// 数量 0 は「持っていない」。ここを取り違えると手仕舞い済みの銘柄を持ち続ける。
	if ctx.HasPosition("BBB") {
		t.Error("数量 0 は保有とみなさない")
	}
	if held := ctx.HeldSymbols(); len(held) != 1 || held[0] != "AAA" {
		t.Errorf("HeldSymbols = %v", held)
	}
}

func TestContextSkipsSymbolsWithNoBarsBeforeAsOf(t *testing.T) {
	bars := barsFrom("AAA", growth(0.001, 10, 100, 0), 1000, nil)
	ctx := NewContext("2024-01-01", map[string][]domain.Bar{"AAA": bars}, nil, decimal.Zero)
	if len(ctx.Symbols()) != 0 {
		t.Errorf("基準日より前に足が無い銘柄は載せない: %v", ctx.Symbols())
	}
}
