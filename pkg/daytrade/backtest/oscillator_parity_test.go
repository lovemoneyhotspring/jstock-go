package backtest_test

import (
	"math"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/internal/fixture"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
)

// 前夜の plan（universe.Build）とバックテストのパネルが、同じ銘柄・同じ日に同じ RSI(2)・
// ストキャス RSI を付ける。窓の起点（係数で揃えた終値の水準）が違っても比は同じなので値は一致する。
func TestPlanAndBacktestOscillatorsMatch(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	// 毎日動く銘柄（ストキャス RSI には 28 本の値動きが要る。buildArchive の銘柄は最後の 10 日しか動かない）
	gap, intra := map[string]float64{}, map[string]float64{}
	for i, d := range days {
		k := d.Format(layout)
		gap[k] = 0.01 * math.Sin(float64(i)*0.7)
		intra[k] = 0.015 * math.Cos(float64(i)*1.3)
	}
	arch, err := fixture.Build(t.TempDir(), days, []fixture.Symbol{
		{Code: "50000", Name: "揺れる", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1200, Turnover: 6e8, MktCap: 9e11, GapOn: gap, IntradayOn: intra},
		{Code: "60000", Name: "揺れる 2", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 800, Turnover: 5e8, MktCap: 8e11, GapOn: intra, IntradayOn: gap},
		{Code: "30000", Name: "動かない", Market: "プライム", ProdCat: "011", Mrgn: "2",
			Base: 1500, Turnover: 3e8, MktCap: 7e11},
	})
	if err != nil {
		t.Fatal(err)
	}
	day, prevDay := days[len(days)-1], days[len(days)-2]
	cfg := parityConfig()
	cfg.Signal.Prefer = config.Prefer{Indicator: config.PreferStochRSI, Max: 0.2, Pool: 20}

	panel, err := backtest.LoadPanel(arch, day, day, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cands, err := universe.Build(arch, day, prevDay, cfg.Universe, cfg.Margin)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]universe.Candidate{}
	for _, c := range cands {
		byCode[c.Code] = c
	}
	same := func(a, b *float64) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return math.Abs(*a-*b) <= 1e-9
	}
	compared, withStoch := 0, 0
	for _, r := range panel.Rows {
		c, ok := byCode[r.Code]
		if !ok {
			continue
		}
		compared++
		if r.StochRSI14 != nil {
			withStoch++
		}
		if !same(r.RSI2, c.RSI2) || !same(r.StochRSI14, c.StochRSI14) {
			t.Errorf("%s: 検証 %v/%v・本番 %v/%v", r.Code, r.RSI2, r.StochRSI14, c.RSI2, c.StochRSI14)
		}
	}
	if compared == 0 || withStoch == 0 {
		t.Fatalf("突き合わせた銘柄 %d・ストキャス RSI のある銘柄 %d（テストが何も確かめていない）", compared, withStoch)
	}
}

// 格子は 1 本目の設定でパネルを作る。1 本目が優先を掛けない設定でも、PanelOptions.Oscillators で値が付く
// （付かないと 2 本目以降の優先が黙って効かない。2026-09-25 のレビュー）。
func TestPanelOscillatorsForGrid(t *testing.T) {
	days := fixture.BusinessDays(start, 60)
	arch := buildArchive(t, days)
	cfg := parityConfig() // 優先を掛けない設定
	day := days[len(days)-1]
	count := func(p *backtest.Panel) int {
		n := 0
		for _, r := range p.Rows {
			if r.RSI2 != nil {
				n++
			}
		}
		return n
	}
	plain, err := backtest.LoadPanelWith(arch, day, day, cfg, backtest.PanelOptions{KeepAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := count(plain); n != 0 {
		t.Fatalf("掛けない設定なのに値が %d 件付いた", n)
	}
	withOsc, err := backtest.LoadPanelWith(arch, day, day, cfg, backtest.PanelOptions{KeepAll: true, Oscillators: true})
	if err != nil {
		t.Fatal(err)
	}
	if count(withOsc) == 0 {
		t.Fatal("Oscillators を渡しても値が付かない")
	}
}
