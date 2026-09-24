package engine

import (
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TestReconcileFrozenSendsNothing は W6 の再現。足が古い保有銘柄はシグナルが出ず目標が無い
// （＝目標 0 株）。以前は order_type = "market" で全株成行売りになった。Frozen に入れた銘柄は
// 売りも買いも出さず、理由を残す。
func TestReconcileFrozenSendsNothing(t *testing.T) {
	positions := map[string]domain.Position{"7203": position("7203", 300, 300)}
	targets := map[string]domain.TargetPosition{
		"6758": {Symbol: "6758", Quantity: d(100), Reason: "新規"},
	}
	settings := defaultSettings()
	settings.OrderType = domain.OrderTypeMarket

	// 凍結しなければ保有の 7203 は全株成行売りになる（直す前の挙動）
	plan, err := Reconcile(targets, positions, nil, nil, nil, settings, nil, "2026-09-24")
	if err != nil {
		t.Fatal(err)
	}
	sold := false
	for _, o := range plan.Orders {
		if o.Symbol == "7203" && o.Side == domain.SideSell && o.Quantity.Equal(d(300)) {
			sold = true
		}
	}
	if !sold {
		t.Fatalf("前提: 目標の無い保有は全株売りになるはず: %+v", plan.Orders)
	}

	settings.Frozen = map[string]string{
		"7203": "足が古い（最終 2026-09-18）",
		"6758": "足を読めない",
	}
	plan, err = Reconcile(targets, positions, nil, nil, nil, settings, nil, "2026-09-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 {
		t.Errorf("判断しない銘柄に注文を出した: %+v", plan.Orders)
	}
	for _, sym := range []string{"7203", "6758"} {
		if !strings.Contains(plan.Skipped[sym], "判断しない") {
			t.Errorf("%s の見送りの理由が無い: %q", sym, plan.Skipped[sym])
		}
	}
}
