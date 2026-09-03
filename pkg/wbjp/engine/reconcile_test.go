package engine

import (
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func d(v int64) decimal.Decimal { return decimal.NewFromInt(v) }

func defaultSettings() ReconcileSettings {
	return ReconcileSettings{
		OrderType:         domain.OrderTypeLimit,
		LimitOffset:       decimal.RequireFromString("0.005"),
		TaxType:           domain.TaxAccountSpecific,
		BlocksSameDaySale: true,
	}
}

// position は保有と売却可能数量を分けて作る。
func position(sym string, qty, available int64) domain.Position {
	return domain.Position{
		Symbol:            sym,
		Quantity:          d(qty),
		AvailableQuantity: d(available),
		CostPrice:         d(1000),
	}
}

func TestReconcileBlocksSameDaySale(t *testing.T) {
	// 当日買い付けた銘柄を売ると差金決済になる。
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: decimal.Zero, Reason: "手仕舞い"},
	}
	positions := map[string]domain.Position{"1306": position("1306", 100, 100)}
	prices := map[string]decimal.Decimal{"1306": d(2000)}
	boughtToday := map[string]struct{}{"1306": {}}

	plan, err := Reconcile(targets, positions, nil, prices, nil, defaultSettings(), boughtToday, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 {
		t.Errorf("当日買付銘柄の売却は見送るべき: %+v", plan.Orders)
	}
	if why := plan.Skipped["1306"]; why == "" {
		t.Error("見送りの理由を残すべき")
	}
}

func TestReconcileAllowsSameDaySaleWhenDisabled(t *testing.T) {
	// 信用取引など差金決済の制約が無い場合は売れる。
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: decimal.Zero},
	}
	positions := map[string]domain.Position{"1306": position("1306", 100, 100)}
	prices := map[string]decimal.Decimal{"1306": d(2000)}

	settings := defaultSettings()
	settings.BlocksSameDaySale = false

	plan, err := Reconcile(targets, positions, nil, prices, nil, settings,
		map[string]struct{}{"1306": {}}, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 1 {
		t.Fatalf("制約が無ければ売れるべき: %+v", plan.Skipped)
	}
}

func TestReconcileClipsToAvailableQuantity(t *testing.T) {
	// 受渡未了分は売れない。保有 300 株でも売却可能が 100 株なら 100 株まで。
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: decimal.Zero},
	}
	positions := map[string]domain.Position{"1306": position("1306", 300, 100)}
	prices := map[string]decimal.Decimal{"1306": d(2000)}

	plan, err := Reconcile(targets, positions, nil, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 1 {
		t.Fatalf("注文が作られるべき: %+v", plan.Skipped)
	}
	if got := plan.Orders[0].Quantity; !got.Equal(d(100)) {
		t.Errorf("売却可能数量に切り下げるべき: %s株", got)
	}
}

func TestReconcileSkipsWhenNothingSellable(t *testing.T) {
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: decimal.Zero},
	}
	// 保有はあるが全て受渡未了。
	positions := map[string]domain.Position{"1306": position("1306", 300, 0)}
	prices := map[string]decimal.Decimal{"1306": d(2000)}

	plan, err := Reconcile(targets, positions, nil, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 {
		t.Errorf("売れる株が無ければ注文を作らない: %+v", plan.Orders)
	}
	if plan.Skipped["1306"] != "売却可能数量が単元株に満たない" {
		t.Errorf("理由 = %q", plan.Skipped["1306"])
	}
}

func TestReconcileLimitRoundsAggressively(t *testing.T) {
	// 買いの指値は「約定しやすい方向」＝上へ丸める。呼値は 2000 円台で 1 円。
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: d(100)},
	}
	prices := map[string]decimal.Decimal{"1306": decimal.RequireFromString("2000.4")}

	plan, err := Reconcile(targets, nil, nil, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 1 {
		t.Fatalf("注文が作られるべき: %+v", plan.Skipped)
	}
	// 2000.4 × 1.005 = 2010.402 → 呼値へ上方向に丸めて 2011。
	got := *plan.Orders[0].LimitPrice
	if !got.Equal(d(2011)) {
		t.Errorf("買いの指値は上へ丸めるべき: %s", got)
	}
}

func TestReconcileSellLimitRoundsDown(t *testing.T) {
	targets := map[string]domain.TargetPosition{
		"1306": {Symbol: "1306", Quantity: decimal.Zero},
	}
	positions := map[string]domain.Position{"1306": position("1306", 100, 100)}
	prices := map[string]decimal.Decimal{"1306": decimal.RequireFromString("2000.4")}

	plan, err := Reconcile(targets, positions, nil, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	// 2000.4 × 0.995 = 1990.398 → 売りは下へ丸めて 1990。
	got := *plan.Orders[0].LimitPrice
	if !got.Equal(d(1990)) {
		t.Errorf("売りの指値は下へ丸めるべき: %s", got)
	}
}

func TestReconcileHonoursTaxType(t *testing.T) {
	targets := map[string]domain.TargetPosition{"1306": {Symbol: "1306", Quantity: d(100)}}
	prices := map[string]decimal.Decimal{"1306": d(2000)}

	settings := defaultSettings()
	settings.TaxType = domain.TaxAccountNISA

	plan, err := Reconcile(targets, nil, nil, prices, nil, settings, nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Orders[0].Request.TaxType; got != domain.TaxAccountNISA {
		t.Errorf("設定した口座区分を使うべき: %s", got)
	}
}

func TestReconcileSkipsSubLotDifference(t *testing.T) {
	targets := map[string]domain.TargetPosition{"1306": {Symbol: "1306", Quantity: d(50)}}
	prices := map[string]decimal.Decimal{"1306": d(2000)}

	plan, err := Reconcile(targets, nil, nil, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 {
		t.Error("単元未満は発注しない")
	}
	if plan.Skipped["1306"] == "" {
		t.Error("理由を残すべき")
	}
}

func TestReconcileAccountsForOpenOrders(t *testing.T) {
	// 実効建玉 = 保有 + 未約定残。既に 100 株の買いが出ていれば追加は出さない。
	targets := map[string]domain.TargetPosition{"1306": {Symbol: "1306", Quantity: d(100)}}
	prices := map[string]decimal.Decimal{"1306": d(2000)}
	open := []domain.Order{{
		ClientOrderID: "既存", Symbol: "1306", Side: domain.SideBuy,
		Quantity: d(100), FilledQuantity: decimal.Zero, Status: domain.OrderStatusSubmitted,
	}}

	plan, err := Reconcile(targets, nil, open, prices, nil, defaultSettings(), nil, "2026-09-03")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Orders) != 0 {
		t.Errorf("未約定残を数えれば追加注文は不要: %+v", plan.Orders)
	}
}
