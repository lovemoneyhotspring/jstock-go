package main

import (
	"path/filepath"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/shopspring/decimal"
)

// TestDailyPnL は W5 の再現。以前は RealizedPnLToday が 0 固定で max_daily_loss が本番で
// 効かなかった。台帳の約定した売りと、保有中の建玉の当日の値動きから損益を出す。
func TestDailyPnL(t *testing.T) {
	rep, err := repo.OpenRepo(filepath.Join(t.TempDir(), "wbjp.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rep.Close() })
	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }
	today := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02")
	if err := rep.StartRun("run-1", today, "uat", "live"); err != nil {
		t.Fatal(err)
	}
	if err := rep.RecordSnapshot("run-1", today, []domain.Position{
		{Symbol: "9984", Quantity: d("100"), CostPrice: d("5000"), LastPrice: d("4800")},
	}); err != nil {
		t.Fatal(err)
	}
	limit := d("4500")
	sell, err := domain.NewOrderRequest("s1", "9984", domain.SideSell, domain.OrderTypeLimit,
		d("100"), &limit, domain.TaxAccountSpecific, "損切り", domain.TradeTypeCash)
	if err != nil {
		t.Fatal(err)
	}
	id := "9/20260924"
	if err := rep.RecordOrder("run-1", sell, string(domain.OrderStatusSubmitted), &id); err != nil {
		t.Fatal(err)
	}
	// 約定単価が返らなかった → 指値（4500）で見積もる: (4500 − 5000) × 100 = −50,000
	if err := rep.UpdateOrder("s1", domain.OrderStatusFilled, d("100"), nil, &id); err != nil {
		t.Fatal(err)
	}

	positions := map[string]domain.Position{
		// 前日終値 1000 → 現値 950: −5,000
		"7203": {Symbol: "7203", Quantity: d("100"), CostPrice: d("900"), LastPrice: d("950")},
		// 当日買付は取得単価から: (2100 − 2000) × 100 = +10,000
		"6758": {Symbol: "6758", Quantity: d("100"), CostPrice: d("2000"), LastPrice: d("2100")},
		// 現値が無い建玉は数えない
		"8306": {Symbol: "8306", Quantity: d("100"), CostPrice: d("1000"), LastPrice: decimal.Zero},
	}
	lastPrices := map[string]decimal.Decimal{"7203": d("1000"), "6758": d("1900"), "8306": d("1000")}
	realized, unrealized, unpriced, err := dailyPnL(rep, today, positions, lastPrices, map[string]struct{}{"6758": {}})
	if err != nil || len(unpriced) != 0 {
		t.Fatalf("err=%v unpriced=%v", err, unpriced)
	}
	if !realized.Equal(d("-50000")) {
		t.Errorf("実現: %s", realized)
	}
	if !unrealized.Equal(d("5000")) {
		t.Errorf("含み: %s", unrealized)
	}
}
