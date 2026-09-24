package engine

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TestReconcileLeavesOutsideUniverseAlone は 2026-09-24 の再点検の再現。ユニバース外の保有
// （手で買った株など）は目標 0 株となり、order_type = "market" なら全株成行売りの注文を
// 作っていた。Universe を渡したら、ユニバース外には目標があっても注文を作らず理由を残す。
func TestReconcileLeavesOutsideUniverseAlone(t *testing.T) {
	positions := map[string]domain.Position{
		"7203": position("7203", 300, 300), // ユニバースの内側（目標 0 → 売る）
		"1234": position("1234", 500, 500), // 手で買った株
	}
	targets := map[string]domain.TargetPosition{
		"1234": {Symbol: "1234", Quantity: d(0), Reason: "地合い弱気のため手仕舞い"},
	}
	settings := defaultSettings()
	settings.OrderType = domain.OrderTypeMarket
	settings.Universe = map[string]struct{}{"7203": {}}

	plan, err := Reconcile(targets, positions, nil, nil, nil, settings, nil, "2026-09-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 1 || plan.Orders[0].Symbol != "7203" || plan.Orders[0].Side != domain.SideSell {
		t.Fatalf("注文はユニバースの内側の 7203 の売りだけ: %+v", plan.Orders)
	}
	if plan.Skipped["1234"] != OutsideUniverseReason {
		t.Errorf("ユニバース外の見送りの理由が無い: %v", plan.Skipped)
	}

	// Universe を渡さない（backtest）なら従来どおり全銘柄を扱う
	settings.Universe = nil
	plan, err = Reconcile(targets, positions, nil, nil, nil, settings, nil, "2026-09-24")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 2 {
		t.Errorf("Universe が nil なのに見送った: %+v", plan.Orders)
	}
}
