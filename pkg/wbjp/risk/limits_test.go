package risk

import (
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

func baseRiskConfig() wbjpcfg.RiskConfig {
	return wbjpcfg.RiskConfig{
		MaxOrderValue:       decimal.NewFromInt(10_000_000),
		MaxOrdersPerDay:     20,
		MaxDailyLoss:        decimal.NewFromInt(1_000_000),
		MaxPositionWeight:   dec("1"),
		MaxGrossExposure:    dec("0.90"),
		MaxPreviewDeviation: dec("0.02"),
	}
}

func buyRequest(symbol, qty, price string) domain.OrderRequest {
	limit := dec(price)
	return domain.OrderRequest{
		Symbol:     symbol,
		Side:       domain.SideBuy,
		OrderType:  domain.OrderTypeLimit,
		Quantity:   dec(qty),
		LimitPrice: &limit,
	}
}

func TestGrossExposureLimitRejects(t *testing.T) {
	mgr := NewRiskManager(baseRiskConfig(), []string{"7203", "6758"})
	ctx := RiskContext{
		Equity:  decimal.NewFromInt(1_000_000),
		Balance: domain.Balance{BuyingPower: decimal.NewFromInt(1_000_000)},
		// 既に 85 万円ぶんの建玉。あと 10 万円買うと 95% で上限 90% 超え。
		Positions: map[string]domain.Position{"6758": pos("6758", "100", "8500", "8500")},
	}

	decision := mgr.Check(buyRequest("7203", "100", "1000"), ctx, nil)
	if decision.Approved {
		t.Fatal("総エクスポージャ上限を超える買いが通ってしまった")
	}
	if !strings.Contains(decision.Reason, "総エクスポージャ") {
		t.Errorf("理由が総エクスポージャによる拒否になっていない: %s", decision.Reason)
	}
}

func TestGrossExposureLimitAllowsWithinLimit(t *testing.T) {
	mgr := NewRiskManager(baseRiskConfig(), []string{"7203", "6758"})
	ctx := RiskContext{
		Equity:    decimal.NewFromInt(1_000_000),
		Balance:   domain.Balance{BuyingPower: decimal.NewFromInt(1_000_000)},
		Positions: map[string]domain.Position{"6758": pos("6758", "100", "7000", "7000")},
	}
	// 70 万 + 10 万 = 80% で上限内。
	if d := mgr.Check(buyRequest("7203", "100", "1000"), ctx, nil); !d.Approved {
		t.Errorf("上限内なのに拒否された: %s", d.Reason)
	}
}

func TestGrossExposureCountsPendingOrders(t *testing.T) {
	// 発注中を数えないと、全部約定した瞬間に上限を突破する。
	mgr := NewRiskManager(baseRiskConfig(), []string{"7203", "6758", "9984"})
	ctx := RiskContext{
		Equity:       decimal.NewFromInt(1_000_000),
		Balance:      domain.Balance{BuyingPower: decimal.NewFromInt(1_000_000)},
		Positions:    map[string]domain.Position{"6758": pos("6758", "100", "5000", "5000")},
		PendingValue: map[string]decimal.Decimal{"9984": decimal.NewFromInt(350_000)},
	}
	// 50 万（建玉）+ 35 万（発注中）+ 10 万（今回）= 95%
	if d := mgr.Check(buyRequest("7203", "100", "1000"), ctx, nil); d.Approved {
		t.Error("発注中を数えずに承認してしまった")
	}
}

func TestGrossExposureSkipsSell(t *testing.T) {
	// 手仕舞いを止めると損失が膨らむだけ。売りには掛けない。
	mgr := NewRiskManager(baseRiskConfig(), []string{"6758"})
	ctx := RiskContext{
		Equity:    decimal.NewFromInt(1_000_000),
		Balance:   domain.Balance{BuyingPower: decimal.Zero},
		Positions: map[string]domain.Position{"6758": pos("6758", "100", "9500", "9500")},
	}
	req := domain.OrderRequest{Symbol: "6758", Side: domain.SideSell, OrderType: domain.OrderTypeMarket, Quantity: dec("100")}
	if d := mgr.Check(req, ctx, nil); !d.Approved {
		t.Errorf("売りが総エクスポージャで止められた: %s", d.Reason)
	}
}

func TestGrossExposureUnsetIsIgnored(t *testing.T) {
	// 設定ファイル経由なら既定 0.90 が入る。0 は「書き忘れ」であって
	// 「全建玉禁止」ではない。
	cfg := baseRiskConfig()
	cfg.MaxGrossExposure = decimal.Zero
	mgr := NewRiskManager(cfg, []string{"7203"})
	ctx := RiskContext{
		Equity:  decimal.NewFromInt(1_000_000),
		Balance: domain.Balance{BuyingPower: decimal.NewFromInt(1_000_000)},
	}
	if d := mgr.Check(buyRequest("7203", "100", "1000"), ctx, nil); !d.Approved {
		t.Errorf("未設定で全発注が止まってしまった: %s", d.Reason)
	}
}
