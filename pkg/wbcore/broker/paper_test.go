package broker

import (
	"errors"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

func TestPaperBroker_Lifecycle(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(1000000), "open")

	// 初期残高チェック
	bal, err := pb.GetBalance()
	if err != nil {
		t.Fatalf("failed to get balance: %v", err)
	}
	if !bal.CashBalance.Equal(decimal.NewFromInt(1000000)) {
		t.Errorf("initial cash = %s, want 1000000", bal.CashBalance)
	}

	// 指値買い発注
	limitPrice := decimal.NewFromInt(2000)
	qty := decimal.NewFromInt(100)
	req, err := domain.NewOrderRequest("order-1", "7203", domain.SideBuy, domain.OrderTypeLimit, qty, &limitPrice, domain.TaxAccountSpecific, "test buy", domain.TradeTypeCash)
	if err != nil {
		t.Fatalf("failed to create order req: %v", err)
	}

	ack, err := pb.Place(req)
	if err != nil {
		t.Fatalf("failed to place order: %v", err)
	}
	if ack.Status != domain.OrderStatusSubmitted {
		t.Errorf("status = %s, want SUBMITTED", ack.Status)
	}

	// 翌日の約定シミュレーション (寄付が 2000 以下なら約定)
	openPrices := map[string]decimal.Decimal{
		"7203": decimal.NewFromInt(1990),
	}
	fills := pb.Settle(openPrices, nil, nil, nil)
	if len(fills) != 1 {
		t.Fatalf("expected 1 fill, got %d", len(fills))
	}
	if !fills[0].Price.Equal(decimal.NewFromInt(1990)) {
		t.Errorf("fill price = %s, want 1990", fills[0].Price)
	}

	// ポジション確認
	positions, err := pb.GetPositions()
	if err != nil {
		t.Fatalf("failed to get positions: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	if !positions[0].Quantity.Equal(qty) {
		t.Errorf("position qty = %s, want %s", positions[0].Quantity, qty)
	}

	// 売り発注
	sellLimit := decimal.NewFromInt(2100)
	sellReq, _ := domain.NewOrderRequest("order-2", "7203", domain.SideSell, domain.OrderTypeLimit, qty, &sellLimit, domain.TaxAccountSpecific, "test sell", domain.TradeTypeCash)
	_, err = pb.Place(sellReq)
	if err != nil {
		t.Fatalf("failed to place sell order: %v", err)
	}

	// 寄付 2110 で約定
	openPrices["7203"] = decimal.NewFromInt(2110)
	sellFills := pb.Settle(openPrices, nil, nil, nil)
	if len(sellFills) != 1 {
		t.Fatalf("expected 1 sell fill, got %d", len(sellFills))
	}

	// 建玉が0になっていること
	positions, _ = pb.GetPositions()
	if len(positions) != 0 {
		t.Errorf("expected 0 positions after sell, got %d", len(positions))
	}

	// 利益が出ていること (売却 2110 - 買付 1990 = 120 * 100 = 12,000円から手数料引いた額)
	if pb.realizedPnL.LessThanOrEqual(decimal.Zero) {
		t.Errorf("expected positive realized pnl, got %s", pb.realizedPnL)
	}
}

// 手数料は定額コース: 現物はその日の合計で段階が決まり、信用は 0 円。
func TestPaperBrokerFlatRateCommission(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990), "9984": decimal.NewFromInt(2000)})
	pb.BeginDay()

	place := func(id, sym string, qty int64, trade domain.TradeType) {
		t.Helper()
		req, err := domain.NewOrderRequest(id, sym, domain.SideBuy, domain.OrderTypeMarket,
			decimal.NewFromInt(qty), nil, domain.TaxAccountSpecific, "test", trade)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	// 滑りを 0 にして代金を読みやすくする
	pb.slippageRate = decimal.Zero

	// 1 件目: 199,000 円 → 20 万円まで 176 円
	place("b1", "7203", 100, domain.TradeTypeCash)
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(176)) {
		t.Fatalf("1 件目の手数料 = %+v, want 176", fills)
	}
	// 2 件目: 合計 399,000 円 → 50 万円まで 253 円。増えるのは差分の 77 円
	place("b2", "9984", 100, domain.TradeTypeCash)
	fills = pb.Settle(map[string]decimal.Decimal{"9984": decimal.NewFromInt(2000)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(77)) {
		t.Fatalf("2 件目の手数料 = %+v, want 77（合計で段階が上がった差分）", fills)
	}
	// 信用は 0 円
	place("m1", "7203", 100, domain.TradeTypeMarginOpen)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.IsZero() {
		t.Fatalf("信用の手数料 = %+v, want 0", fills)
	}
	// 日が変わると合計は戻る
	pb.BeginDay()
	place("b3", "7203", 100, domain.TradeTypeCash)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(1990)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Fee.Equal(decimal.NewFromInt(176)) {
		t.Fatalf("翌日の 1 件目の手数料 = %+v, want 176", fills)
	}
}

// 立会いの無かった銘柄の注文は失効させず、次の立会いで約定させる。
func TestPaperBrokerExpiresOnlyTradedSymbols(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000), "SPY": decimal.NewFromInt(500)})
	for _, sym := range []string{"7203", "SPY"} {
		req, _ := domain.NewOrderRequest("o-"+sym, sym, domain.SideBuy, domain.OrderTypeMarket,
			decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	// 米国だけが立ち会った日: SPY の注文は失効、7203 の注文は残る
	pb.ExpireOpenOrdersFor(map[string]struct{}{"SPY": {}})
	open, _ := pb.GetOpenOrders()
	if len(open) != 1 || open[0].Symbol != "7203" {
		t.Fatalf("残る注文 = %+v, want 7203 だけ", open)
	}
	// 次の東証の立会いで約定する
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000)}, nil, nil, nil)
	if len(fills) != 1 || fills[0].Symbol != "7203" {
		t.Fatalf("次の立会いで約定していない: %+v", fills)
	}
	// nil なら全部失効
	req, _ := domain.NewOrderRequest("o-2", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if _, err := pb.Place(req); err != nil {
		t.Fatal(err)
	}
	pb.ExpireOpenOrders()
	if open, _ := pb.GetOpenOrders(); len(open) != 0 {
		t.Errorf("全銘柄の失効で注文が残っている: %+v", open)
	}
}

// 逆指値は条件に触れるまで約定せず、触れたら発火した値段で成行になる。
// 発火前は条件を訂正でき、発火後はできない。
func TestPaperBrokerStopOrder(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "intrabar")
	pb.slippageRate = decimal.Zero
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)})
	buy, _ := domain.NewOrderRequest("b", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "entry", domain.TradeTypeCash)
	if _, err := pb.Place(buy); err != nil {
		t.Fatal(err)
	}
	pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)}, nil, nil, nil)

	sell, _ := domain.NewOrderRequest("s", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "stop", domain.TradeTypeCash)
	stop, err := sell.WithStop(decimal.NewFromInt(2400), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pb.Place(stop); err != nil {
		t.Fatal(err)
	}

	// 安値が条件に届かない日は何もしない
	hi, lo := decimal.NewFromInt(2550), decimal.NewFromInt(2450)
	fills := pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)},
		map[string]decimal.Decimal{"7203": hi}, map[string]decimal.Decimal{"7203": lo}, nil)
	if len(fills) != 0 {
		t.Fatalf("条件に触れていないのに約定した: %+v", fills)
	}
	// 発火前は条件を引き上げられる
	if err := pb.CorrectStop("s", nil, domain.StopSpec{Trigger: decimal.NewFromInt(2450)}); err != nil {
		t.Fatalf("発火前の訂正が拒否された: %v", err)
	}
	open, _ := pb.GetOpenOrders()
	if len(open) != 1 || open[0].Stop == nil || !open[0].Stop.Trigger.Equal(decimal.NewFromInt(2450)) {
		t.Fatalf("訂正が反映されていない: %+v", open)
	}

	// 安値が条件に触れた日は条件価格で成行約定
	lo = decimal.NewFromInt(2440)
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)},
		map[string]decimal.Decimal{"7203": hi}, map[string]decimal.Decimal{"7203": lo}, nil)
	if len(fills) != 1 || !fills[0].Price.Equal(decimal.NewFromInt(2450)) {
		t.Fatalf("逆指値の約定 = %+v, want 2450 で 1 件", fills)
	}
	if err := pb.CorrectStop("s", nil, domain.StopSpec{Trigger: decimal.NewFromInt(2300)}); err == nil {
		t.Error("約定済みの逆指値の訂正を通している")
	}

	// 寄付で条件を割って寄れば寄付の値段（ギャップ分は不利）。別 ID で建て直す
	buy2, _ := domain.NewOrderRequest("b2", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "entry", domain.TradeTypeCash)
	if _, err := pb.Place(buy2); err != nil {
		t.Fatal(err)
	}
	pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2500)}, nil, nil, nil)
	sell2, _ := domain.NewOrderRequest("s2", "7203", domain.SideSell, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "stop", domain.TradeTypeCash)
	stop2, _ := sell2.WithStop(decimal.NewFromInt(2400), nil)
	if _, err := pb.Place(stop2); err != nil {
		t.Fatal(err)
	}
	fills = pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2300)}, nil, nil, nil)
	if len(fills) != 1 || !fills[0].Price.Equal(decimal.NewFromInt(2300)) {
		t.Fatalf("ギャップで抜けた逆指値の約定 = %+v, want 寄付 2300", fills)
	}
}

// 終わった注文の取消は実機（立花証券）と同じく業務エラー。nil を返すと
// 「取消せた」と読まれ、約定済みの玉が無いものとして扱われる。
func TestPaperBrokerCancelTerminalOrderIsAnError(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000)})
	req, _ := domain.NewOrderRequest("o-1", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if _, err := pb.Place(req); err != nil {
		t.Fatal(err)
	}
	// 生きている注文は取消せる
	if err := pb.Cancel("o-1", nil); err != nil {
		t.Fatalf("未約定の取消: %v", err)
	}
	// 取消済み（終局）の取消はエラー
	if err := pb.Cancel("o-1", nil); err == nil {
		t.Error("取消済みの注文の取消が nil で通った")
	}
	req2, _ := domain.NewOrderRequest("o-2", "7203", domain.SideBuy, domain.OrderTypeMarket,
		decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
	if _, err := pb.Place(req2); err != nil {
		t.Fatal(err)
	}
	pb.Settle(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000)}, nil, nil, nil)
	if err := pb.Cancel("o-2", nil); err == nil {
		t.Error("約定済みの注文の取消が nil で通った")
	}
	if err := pb.Cancel("none", nil); err == nil {
		t.Error("無い注文の取消が nil で通った")
	}
}

// 売買単位は与えた銘柄だけ返す。空売り価格規制（50 単元超の成行売建）は与えていない銘柄を既定 100 株で見る。
func TestPaperBrokerLotSizesAndShortSaleRule(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	// 与えていない銘柄は「分からない」としてキーごと省く（既定の 100 株を埋めない）
	got := pb.LotSizes([]string{"7203", "1629", ""})
	if len(got) != 0 {
		t.Errorf("与えていない売買単位 = %v, want 空", got)
	}
	pb.SetLotSizes(map[string]decimal.Decimal{"1629": decimal.NewFromInt(10), "bad": decimal.Zero})
	got = pb.LotSizes([]string{"7203", "1629", "bad"})
	if len(got) != 1 || !got["1629"].Equal(decimal.NewFromInt(10)) {
		t.Errorf("与えた売買単位 = %v, want 1629 だけ 10", got)
	}

	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000), "1629": decimal.NewFromInt(2000)})
	short := func(id, sym string, qty int64, typ domain.OrderType, limit *decimal.Decimal) error {
		req, err := domain.NewOrderRequest(id, sym, domain.SideSell, typ, decimal.NewFromInt(qty), limit,
			domain.TaxAccountSpecific, "test", domain.TradeTypeMarginOpen)
		if err != nil {
			t.Fatal(err)
		}
		_, err = pb.Place(req)
		return err
	}
	var rejected *OrderRejectedError
	// 単位 100: 5,000 株までは成行で通り、5,100 株は拒否
	if err := short("s1", "7203", 5000, domain.OrderTypeMarket, nil); err != nil {
		t.Errorf("50 単元の成行売建が弾かれた: %v", err)
	}
	if err := short("s2", "7203", 5100, domain.OrderTypeMarket, nil); !errors.As(err, &rejected) {
		t.Errorf("51 単元の成行売建が通った: %v", err)
	}
	// 単位 10: 50 単元 = 500 株まで（既定の 100 株単位で数えると 5,000 株まで通ってしまう）
	if err := short("s3", "1629", 500, domain.OrderTypeMarket, nil); err != nil {
		t.Errorf("単位 10 の銘柄で 500 株の成行売建が弾かれた: %v", err)
	}
	if err := short("s3b", "1629", 510, domain.OrderTypeMarket, nil); !errors.As(err, &rejected) {
		t.Errorf("単位 10 の銘柄で 510 株の成行売建が通った: %v", err)
	}
	// 指値なら数量に関係なく通る
	limit := decimal.NewFromInt(2000)
	if err := short("s4", "7203", 10000, domain.OrderTypeLimit, &limit); err != nil {
		t.Errorf("指値の売建が弾かれた: %v", err)
	}
}

// 待機資金の利息は年率を 360 日で日割りする（T-Bill の慣行）。
func TestPaperBrokerAccrueInterest(t *testing.T) {
	p := NewPaperBroker(decimal.NewFromInt(3_600_000), "open")

	// 年 5% を 36 日 → 3,600,000 × 0.05 × 36/360 = 18,000
	got := p.AccrueInterest(decimal.RequireFromString("0.05"), 36)
	if !got.Equal(decimal.NewFromInt(18_000)) {
		t.Errorf("利息 = %s, want 18000", got)
	}
	bal, _ := p.GetBalance()
	if !bal.CashBalance.Equal(decimal.NewFromInt(3_618_000)) {
		t.Errorf("残高 = %s, want 3618000", bal.CashBalance)
	}

	// 金利ゼロ・日数ゼロでは何もしない
	if !p.AccrueInterest(decimal.Zero, 30).IsZero() {
		t.Error("金利ゼロで利息が付きました")
	}
	if !p.AccrueInterest(decimal.RequireFromString("0.05"), 0).IsZero() {
		t.Error("日数ゼロで利息が付きました")
	}
}

// 買付余力が 1 銘柄ぶんしか無い日に 3 銘柄の買いを出すと、約定するのは 1 つだけ
// （発注時の検査は 1 本ずつ余力と比べるので 3 本とも通る）。どれが約定するかが
// map の走査順（実行ごとに変わる）で決まってはいけない——同じ設定のバックテストが
// 走らせるたびに違う値を返していた原因。
func TestPaperBrokerSettlesOpenOrdersInDeterministicOrder(t *testing.T) {
	place := func(pb *PaperBroker, id, symbol string, side domain.Side) {
		t.Helper()
		req, err := domain.NewOrderRequest(id, symbol, side, domain.OrderTypeMarket, decimal.NewFromInt(100), nil,
			domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	prices := map[string]decimal.Decimal{
		"9984": decimal.NewFromInt(1000), "7203": decimal.NewFromInt(1000), "6758": decimal.NewFromInt(1000),
	}

	for i := 0; i < 20; i++ {
		pb := NewPaperBroker(decimal.NewFromInt(150_000), "open") // 1 銘柄（10 万円 + 手数料）ぶん
		pb.Mark(prices)
		place(pb, "o-9984", "9984", domain.SideBuy)
		place(pb, "o-7203", "7203", domain.SideBuy)
		place(pb, "o-6758", "6758", domain.SideBuy)
		fills := pb.Settle(prices, nil, nil, nil)
		if len(fills) != 1 || fills[0].Symbol != "6758" {
			t.Fatalf("銘柄コード順に約定するはず（6758）: %+v", fills)
		}
	}

}

// 寄成は始値を決める板寄せで約定するので、寄値ちょうどで建つ。
// ザラ場の成行は最良気配に当たるので滑りを払う——dry-run で両者を混ぜないための区別。
func TestPaperBrokerOpeningConditionFillsAtOpen(t *testing.T) {
	pb := NewPaperBroker(decimal.NewFromInt(10_000_000), "open")
	pb.Mark(map[string]decimal.Decimal{"7203": decimal.NewFromInt(2000)})
	place := func(id string, condition domain.OrderCondition) {
		req, err := domain.NewOrderRequest(id, "7203", domain.SideBuy, domain.OrderTypeMarket,
			decimal.NewFromInt(100), nil, domain.TaxAccountSpecific, "test", domain.TradeTypeCash)
		if err != nil {
			t.Fatal(err)
		}
		if req, err = req.WithCondition(condition); err != nil {
			t.Fatal(err)
		}
		if _, err := pb.Place(req); err != nil {
			t.Fatal(err)
		}
	}
	place("moo", domain.ConditionOpening)
	place("mkt", domain.ConditionNone)

	open := decimal.NewFromInt(2000)
	pb.Settle(map[string]decimal.Decimal{"7203": open}, nil, nil, nil)

	moo, err := pb.GetOrder("moo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if moo.AvgFillPrice == nil || !moo.AvgFillPrice.Equal(open) {
		t.Errorf("寄成の約定値 = %v, want %s（寄値ちょうど）", moo.AvgFillPrice, open)
	}
	mkt, err := pb.GetOrder("mkt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if mkt.AvgFillPrice == nil || !mkt.AvgFillPrice.GreaterThan(open) {
		t.Errorf("ザラ場の成行買いの約定値 = %v, want > %s（滑りを払う）", mkt.AvgFillPrice, open)
	}
}
