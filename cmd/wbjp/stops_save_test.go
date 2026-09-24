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

// TestDecideStopExitsSavesTakeProfitChanges は W2 と 2026-09-24 のレビューの再現。
//
//   - 利確の売りを出しただけの回は ScaledOut を保存しない。売りが約定しなかったら次の回に
//     同じ残り株数の利確が出る（以前は出した回に保存され、二度と利確が出なかった）
//   - 保有が減ったのを見た回に、利確で変えたストップ（建値への引き上げ・ScaledOut）が台帳に
//     残る（以前は SyncStops が利確の前にあり、変更が次の回に残らなかった）
func TestDecideStopExitsSavesTakeProfitChanges(t *testing.T) {
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	logger, _ := logging.NewLogger("wbjp", "uat", "r", "test", "")

	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	initStop, initQty := d("900"), d("100")
	fresh := func() *risk.StopBook {
		return risk.NewStopBook(map[string]*risk.Stop{"7203": {
			Symbol: "7203", EntryPrice: d("1000"), StopPrice: d("900"), CreatedOn: "2026-09-01",
			ATRMultiple: d("2"), InitialStopPrice: &initStop, InitialQuantity: &initQty,
		}})
	}
	// 台帳から読み直す（run の各回の始まりと同じ）
	reload := func() *risk.StopBook {
		saved, err := rep.GetStops()
		if err != nil {
			t.Fatal(err)
		}
		m := make(map[string]*risk.Stop, len(saved))
		for sym, st := range saved {
			m[sym] = &risk.Stop{Symbol: st.Symbol, StopPrice: st.StopPrice, EntryPrice: st.EntryPrice,
				CreatedOn: st.CreatedOn, ATRMultiple: st.ATRMultiple, InitialStopPrice: st.InitialStopPrice,
				InitialQuantity: st.InitialQuantity, ScaledOut: st.ScaledOut}
		}
		return risk.NewStopBook(m)
	}
	takeProfitR := d("2")
	cfg := wbjpcfg.StopsConfig{TakeProfitR: &takeProfitR, TakeProfitFraction: d("0.5"), TrendExitKind: "sma"}
	in := func(held string) risk.ExitInputs {
		return risk.ExitInputs{
			Closes:     map[string]decimal.Decimal{"7203": d("1250")},
			Quantities: map[string]decimal.Decimal{"7203": d(held)},
			LotSizes:   map[string]decimal.Decimal{"7203": d("1")},
			AsOf:       "2026-09-24",
		}
	}

	// dry-run は台帳を書かない
	decideStopExits(rep, fresh(), cfg, in("100"), false, logger)
	if saved, err := rep.GetStops(); err != nil || len(saved) != 0 {
		t.Fatalf("dry-run がストップを保存した: %+v err=%v", saved, err)
	}

	// 1 回目: 利確の売りを出す。ScaledOut はまだ保存しない
	targets := decideStopExits(rep, fresh(), cfg, in("100"), true, logger)
	if len(targets) != 1 || !targets[0].Quantity.Equal(d("50")) {
		t.Errorf("利確の目標（残り 50 株）: %+v", targets)
	}
	if st := mustStop(t, rep); st.ScaledOut || !st.StopPrice.Equal(d("900")) {
		t.Errorf("売りを出しただけで利確済みを保存した: %+v", st)
	}

	// 2 回目: 売りが約定しなかった（100 株のまま）→ 同じ利確がもう一度出る
	targets = decideStopExits(rep, reload(), cfg, in("100"), true, logger)
	if len(targets) != 1 || !targets[0].Quantity.Equal(d("50")) {
		t.Errorf("約定しなかった利確が出し直されない: %+v", targets)
	}

	// 3 回目: 50 株まで減った → 確定して保存（建値への引き上げ・ScaledOut）
	targets = decideStopExits(rep, reload(), cfg, in("50"), true, logger)
	if len(targets) != 1 || !targets[0].Quantity.Equal(d("50")) {
		t.Errorf("約定後は残り玉の維持（50 株）: %+v", targets)
	}
	if st := mustStop(t, rep); !st.ScaledOut || !st.StopPrice.Equal(d("1000")) {
		t.Errorf("利確で変えたストップが保存されていない: %+v", st)
	}
}

func mustStop(t *testing.T, rep *repo.Repo) repo.StopRecord {
	t.Helper()
	saved, err := rep.GetStops()
	if err != nil {
		t.Fatal(err)
	}
	st, ok := saved["7203"]
	if !ok {
		t.Fatalf("ストップが保存されていない: %+v", saved)
	}
	return st
}
