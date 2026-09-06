package strategy

import (
	"math"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestMarginBookRespectsPublicationLag(t *testing.T) {
	book := NewMarginBook(map[string][]MarginRecord{
		"AAA": {
			{Date: "2025-01-17", Long: 100, Short: 50}, // 金曜。水曜 01-22 から見える
			{Date: "2025-01-10", Long: 80, Short: 40},
		},
	})
	if _, ok := book.AsOf("AAA", "2025-01-14"); ok {
		t.Error("01-10 の残高は 01-15 より前には見えないはず")
	}
	if r, ok := book.AsOf("AAA", "2025-01-21"); !ok || r.Date != "2025-01-10" {
		t.Errorf("01-21 には 01-10 の残高が見えるはず: %+v %t", r, ok)
	}
	if r, ok := book.AsOf("AAA", "2025-01-22"); !ok || r.Date != "2025-01-17" {
		t.Errorf("01-22 には 01-17 の残高が見えるはず: %+v %t", r, ok)
	}
	if r, ok := book.AsOf("AAA", ""); !ok || r.Date != "2025-01-17" {
		t.Errorf("asOf 空なら最新: %+v", r)
	}
	if _, ok := book.AsOf("ZZZ", "2025-01-22"); ok {
		t.Error("無い銘柄は ok=false")
	}
	var nilBook *MarginBook
	if _, ok := nilBook.AsOf("AAA", "2025-01-22"); ok {
		t.Error("nil の帳簿は ok=false")
	}
}

func TestMarginRecordRatio(t *testing.T) {
	if r := (MarginRecord{Long: 300, Short: 100}).Ratio(); r != 3 {
		t.Errorf("倍率 = %g, 期待 3", r)
	}
	if r := (MarginRecord{Long: 300, Short: 0}).Ratio(); !math.IsInf(r, 1) {
		t.Errorf("売残ゼロは +Inf: %g", r)
	}
	if r := (MarginRecord{}).Ratio(); !math.IsNaN(r) {
		t.Errorf("両方ゼロは NaN: %g", r)
	}
}

// marginUniverse は 5 銘柄の足と、倍率 0.5 / 1 / 2 / 4 / 8 の信用残。
func marginUniverse() (map[string][]domain.Bar, *MarginBook) {
	bars := map[string][]domain.Bar{}
	records := map[string][]MarginRecord{}
	ratios := map[string]float64{"AAA": 0.5, "BBB": 1, "CCC": 2, "DDD": 4, "EEE": 8}
	for sym, ratio := range ratios {
		bars[sym] = barsFrom(sym, growth(0.001, 30, 100, 0), 1000, nil)
		records[sym] = []MarginRecord{{Date: "2024-12-20", Long: ratio * 100, Short: 100}}
	}
	bars["NOMARGIN"] = barsFrom("NOMARGIN", growth(0.001, 30, 100, 0), 1000, nil)
	return bars, NewMarginBook(records)
}

func TestContextMarginRatioRank(t *testing.T) {
	bars, book := marginUniverse()
	ctx := NewUniverse(bars).SetMargin(book).At("", nil, decimal.Zero)

	want := map[string]float64{"AAA": 0.1, "BBB": 0.3, "CCC": 0.5, "DDD": 0.7, "EEE": 0.9}
	for sym, w := range want {
		got, ok := ctx.MarginRatioRank(sym)
		if !ok || math.Abs(got-w) > 1e-9 {
			t.Errorf("%s の分位 = %g (%t), 期待 %g", sym, got, ok, w)
		}
	}
	if _, ok := ctx.MarginRatioRank("NOMARGIN"); ok {
		t.Error("信用残の無い銘柄は ok=false")
	}
	if _, ok := NewContext("", bars, nil, decimal.Zero).MarginRatioRank("AAA"); ok {
		t.Error("信用残を載せていない Context では ok=false")
	}
}

func TestContextMarginRankTiesShareTheMiddle(t *testing.T) {
	bars := map[string][]domain.Bar{}
	records := map[string][]MarginRecord{}
	for _, sym := range []string{"A", "B", "C", "D"} {
		bars[sym] = barsFrom(sym, growth(0.001, 30, 100, 0), 1000, nil)
		records[sym] = []MarginRecord{{Date: "2024-12-20", Long: 200, Short: 100}}
	}
	records["D"] = []MarginRecord{{Date: "2024-12-20", Long: 800, Short: 100}}
	ctx := NewUniverse(bars).SetMargin(NewMarginBook(records)).At("", nil, decimal.Zero)
	for _, sym := range []string{"A", "B", "C"} {
		if r, _ := ctx.MarginRatioRank(sym); math.Abs(r-0.375) > 1e-9 {
			t.Errorf("同順位 3 銘柄は (0 + 1.5)/4 = 0.375 のはず: %s %g", sym, r)
		}
	}
	if r, _ := ctx.MarginRatioRank("D"); math.Abs(r-0.875) > 1e-9 {
		t.Errorf("最上位は (3 + 0.5)/4 = 0.875 のはず: %g", r)
	}
}
