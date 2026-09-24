package main

import (
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/shopspring/decimal"
)

// TestDecideStopExitsSavesTakeProfitChanges は W2 の再現。利確で変えたストップ（建値への
// 引き上げ・ScaledOut）が台帳に残る。以前は SyncStops が利確の前にあり、次の回は利確前の
// ストップから始まっていた（同じ建玉をもう一度利確し、建値ストップも消える）。
func TestDecideStopExitsSavesTakeProfitChanges(t *testing.T) {
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")

	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	initStop, initQty := d("900"), d("100")
	book := risk.NewStopBook(map[string]*risk.Stop{"7203": {
		Symbol: "7203", EntryPrice: d("1000"), StopPrice: d("900"), CreatedOn: "2026-09-01",
		ATRMultiple: d("2"), InitialStopPrice: &initStop, InitialQuantity: &initQty,
	}})
	takeProfitR := d("2")
	cfg := wbjpcfg.StopsConfig{TakeProfitR: &takeProfitR, TakeProfitFraction: d("0.5"), TrendExitKind: "sma"}
	in := risk.ExitInputs{
		Closes:     map[string]decimal.Decimal{"7203": d("1250")},
		Quantities: map[string]decimal.Decimal{"7203": d("100")},
		LotSizes:   map[string]decimal.Decimal{"7203": d("1")},
		AsOf:       "2026-09-24",
	}

	// dry-run は台帳を書かない
	decideStopExits(rep, book, cfg, in, false, logger)
	if saved, err := rep.GetStops(); err != nil || len(saved) != 0 {
		t.Fatalf("dry-run がストップを保存した: %+v err=%v", saved, err)
	}

	book = risk.NewStopBook(map[string]*risk.Stop{"7203": {
		Symbol: "7203", EntryPrice: d("1000"), StopPrice: d("900"), CreatedOn: "2026-09-01",
		ATRMultiple: d("2"), InitialStopPrice: &initStop, InitialQuantity: &initQty,
	}})
	targets := decideStopExits(rep, book, cfg, in, true, logger)
	if len(targets) != 1 || !targets[0].Quantity.Equal(d("50")) {
		t.Errorf("利確の目標（残り 50 株）: %+v", targets)
	}
	saved, err := rep.GetStops()
	if err != nil {
		t.Fatal(err)
	}
	st, ok := saved["7203"]
	if !ok || !st.ScaledOut || !st.StopPrice.Equal(d("1000")) {
		t.Errorf("利確で変えたストップが保存されていない: %+v", st)
	}
}
