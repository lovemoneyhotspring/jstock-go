package broker

import (
	"strings"
	"testing"
)

func TestWithPriceColumns(t *testing.T) {
	if got := withPriceColumns(""); got != MarketPriceColumns {
		t.Errorf("空 = %q, want %q", got, MarketPriceColumns)
	}
	// 板の列に時価の列が足りなければ後ろに足し、重ねない
	got := withPriceColumns("pQAP, pQBP,pGAP1,pQAP")
	for _, c := range strings.Split(MarketPriceColumns, ",") {
		if strings.Count(","+got+",", ","+c+",") != 1 {
			t.Errorf("%s が 1 回だけでない: %q", c, got)
		}
	}
	if !strings.HasPrefix(got, "pQAP,pQBP,pGAP1,") {
		t.Errorf("板の列の並びが崩れた: %q", got)
	}
}

// 板の列で取った行からも、6 列のときと同じ時価ができる（並べ方を変えないため）。
func TestMarketPricesOfIgnoresExtraColumns(t *testing.T) {
	base := map[string]any{"sIssueCode": "7203", "pDOP": "", "pDPP": "2500", "pPRP": "2550", "tDPP:T": "08:59", "pQBP": "2499", "pQAP": "2501"}
	wide := map[string]any{"pGAP1": "2502", "pGAV1": "300", "pIEP": "2500"}
	for k, v := range base {
		wide[k] = v
	}
	a, b := marketPricesOf([]map[string]any{base})["7203"], marketPricesOf([]map[string]any{wide})["7203"]
	if !a.Last.Equal(b.Last) || !a.Bid.Equal(b.Bid) || !a.Ask.Equal(b.Ask) || !a.PrevClose.Equal(b.PrevClose) || !a.At.Equal(b.At) {
		t.Fatalf("列を増やすと時価が変わった: %+v vs %+v", a, b)
	}
}
