package risk

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// TestDailyLossCountsUnrealizedAndUnknown は W5。当日の損失は実現と含みの合計で見て、
// 確かめられなければ新規の買いを止める（売りは止めない）。
func TestDailyLossCountsUnrealizedAndUnknown(t *testing.T) {
	mgr := NewRiskManager(baseRiskConfig(), []string{"7203"}) // 上限 100 万円
	base := RiskContext{
		Equity:  decimal.NewFromInt(10_000_000),
		Balance: domain.Balance{BuyingPower: decimal.NewFromInt(10_000_000)},
	}
	buy := buyRequest("7203", "100", "1000")
	sell := buyRequest("7203", "100", "1000")
	sell.Side = domain.SideSell

	ctx := base
	ctx.RealizedPnLToday = decimal.NewFromInt(-400_000)
	if d := mgr.Check(buy, ctx, nil); !d.Approved {
		t.Fatalf("上限の内側で止めた: %s", d.Reason)
	}
	// 含み損を足すと上限に届く
	ctx.UnrealizedPnLToday = decimal.NewFromInt(-600_000)
	if d := mgr.Check(buy, ctx, nil); d.Approved {
		t.Error("実現 + 含みで上限に達しているのに買いを通した")
	}

	ctx = base
	ctx.DailyPnLUnknown = true
	ctx.Positions = map[string]domain.Position{"7203": {
		Symbol: "7203", Quantity: decimal.NewFromInt(100), AvailableQuantity: decimal.NewFromInt(100),
	}}
	if d := mgr.Check(buy, ctx, nil); d.Approved {
		t.Error("当日の損益が分からないのに買いを通した")
	}
	if d := mgr.Check(sell, ctx, nil); !d.Approved {
		t.Errorf("売り（手仕舞い）まで止めた: %s", d.Reason)
	}
}
