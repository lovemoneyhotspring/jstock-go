package execute

import (
	"fmt"
	"strings"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/shopspring/decimal"
)

type PlannedOrder struct {
	Symbol     string
	Amount     decimal.Decimal
	Quantity   decimal.Decimal
	LimitPrice *decimal.Decimal
	Request    *domain.OrderRequest
	Reason     string
	Note       string
}

func BrokerSymbol(symbol string) string {
	s := strings.TrimPrefix(symbol, "^")
	return strings.TrimSuffix(s, ".T")
}

// PlanOrders は最新の足データと台帳情報から本日の発注計画を立てる。
func PlanOrders(
	cfg *accumcfg.AccumConfig,
	barStore *data.BarStore,
	led *ledger.Ledger,
	now time.Time,
) ([]PlannedOrder, error) {
	var orders []PlannedOrder

	todayJST := clock.ToZone(now, clock.Tokyo).Format("2006-01-02")
	monthStart := todayJST[:7] + "-01"
	monthTime, _ := time.Parse("2006-01-02", monthStart)

	for _, entry := range cfg.Tactics {
		if !entry.IsEnabled() {
			continue
		}

		var tactic tactics.Tactic
		switch entry.Tactic {
		case "constant":
			tactic = &tactics.Constant{}
		case "bear_stack":
			tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
		case "stack_ladder":
			tactic = tactics.NewStackLadder(nil, entry.Fast, entry.Mid, entry.Slow)
		default:
			tactic = &tactics.Constant{}
		}

		for _, sym := range entry.Symbols {
			bars, err := barStore.Read(sym, "", "")
			if err != nil || len(bars) == 0 {
				orders = append(orders, PlannedOrder{
					Symbol: sym,
					Note:   "足データなし",
				})
				continue
			}

			// 確定足（前日以前）で計画を計算
			var completed []domain.Bar
			for _, b := range bars {
				if b.Date < todayJST {
					completed = append(completed, b)
				}
			}
			if len(completed) == 0 {
				continue
			}

			p, err := plan.BuildPlan(completed, tactic, entry.MonthlyBudget)
			if err != nil || len(p.Rows) == 0 {
				continue
			}

			// 今月の目標額
			targetThisMonth := decimal.Zero
			for _, r := range p.Rows {
				if strings.HasPrefix(r.Date, todayJST[:7]) {
					targetThisMonth = targetThisMonth.Add(r.Amount)
				}
			}

			// 発注済み額を台帳から引く
			bSym := BrokerSymbol(sym)
			placed, err := led.PlacedAmount(bSym, monthTime)
			if err != nil {
				placed = decimal.Zero
			}

			due := targetThisMonth.Sub(placed)
			if due.LessThanOrEqual(decimal.Zero) {
				continue // 今月分は発注済み
			}

			lastBar := completed[len(completed)-1]
			lastPrice := lastBar.Close

			// 単元株数（既定100、オーバーライドあり）
			lotSize := marketrules.DefaultLotSize
			if ov, ok := cfg.Execution.LotSizeOverrides[bSym]; ok && ov > 0 {
				lotSize = decimal.NewFromInt(int64(ov))
			}

			// 指値価格の計算 (終値 * (1 + offset))
			offset := decimal.RequireFromString("0.01")
			if cfg.Execution.LimitOffset != "" {
				if d, err := decimal.NewFromString(cfg.Execution.LimitOffset); err == nil {
					offset = d
				}
			}

			limitPrice := lastPrice.Mul(decimal.NewFromInt(1).Add(offset))
			snappedPrice, _ := marketrules.SnapToTick(limitPrice, domain.SideBuy, false, marketrules.RoundingConservative)

			// 株数 = floor(due / snappedPrice) を lotSize で切り捨て
			rawQty := due.Div(snappedPrice).Floor()
			qty, _ := marketrules.RoundToLot(rawQty, lotSize)

			if qty.LessThanOrEqual(decimal.Zero) {
				orders = append(orders, PlannedOrder{
					Symbol: bSym,
					Amount: due,
					Reason: p.Rows[len(p.Rows)-1].Reason,
					Note:   fmt.Sprintf("単元株数（%s株）に満たないため見送り", lotSize),
				})
				continue
			}

			orderType := domain.OrderTypeLimit
			if cfg.Execution.OrderType == "market" {
				orderType = domain.OrderTypeMarket
			}

			taxType := domain.TaxAccountSpecific
			if cfg.Execution.TaxAccountType != "" {
				taxType = domain.TaxAccountType(cfg.Execution.TaxAccountType)
			}

			orderID := domain.MakeClientOrderID(todayJST, bSym, domain.SideBuy, qty)
			req, err := domain.NewOrderRequest(
				orderID,
				bSym,
				domain.SideBuy,
				orderType,
				qty,
				&snappedPrice,
				taxType,
				p.Rows[len(p.Rows)-1].Reason,
				domain.TradeTypeCash,
			)
			if err != nil {
				continue
			}

			orders = append(orders, PlannedOrder{
				Symbol:     bSym,
				Amount:     due,
				Quantity:   qty,
				LimitPrice: &snappedPrice,
				Request:    &req,
				Reason:     req.Reason,
			})
		}
	}

	return orders, nil
}

// RunAccumulation は積立の実行サイクル（照会 → 計画 → 発注/記録）を行う。
func RunAccumulation(
	cfg *accumcfg.AccumConfig,
	b broker.Broker,
	barStore *data.BarStore,
	led *ledger.Ledger,
	logger *logging.Logger,
	isLive bool,
) error {
	now := clock.NowUTC()
	todayJST := clock.ToZone(now, clock.Tokyo).Format("2006-01-02")
	monthStart := todayJST[:7] + "-01"

	// 1. オープン注文の約定状況をブローカーに照会
	openOrders, err := led.OpenOrders()
	if err == nil {
		for _, oo := range openOrders {
			bo, err := b.GetOrder(oo.ClientOrderID, oo.BrokerOrderID)
			if err == nil && bo != nil {
				if bo.Status.IsTerminal() {
					_ = led.UpdateStatus(oo.ClientOrderID, string(bo.Status), &bo.FilledQuantity, bo.AvgFillPrice)
					logger.Info("accum.fill", fmt.Sprintf("%s 注文確定: %s (%s約定)", oo.Symbol, bo.Status, bo.FilledQuantity))
				}
			}
		}
	}

	// 2. 本日の発注計画
	planned, err := PlanOrders(cfg, barStore, led, now)
	if err != nil {
		return err
	}

	// 3. 発注処理
	bal, err := b.GetBalance()
	buyingPower := decimal.Zero
	if err == nil && bal != nil {
		buyingPower = bal.BuyingPower
	}

	for _, po := range planned {
		if po.Request == nil {
			if po.Note != "" {
				logger.Info("accum.skip", fmt.Sprintf("%s: %s", po.Symbol, po.Note))
			}
			continue
		}

		req := *po.Request
		if led.WasPlaced(req.ClientOrderID) {
			logger.Info("accum.skip", fmt.Sprintf("%s: 既に発注済み (ID: %s)", po.Symbol, req.ClientOrderID))
			continue
		}

		// 見積りと買付余力チェック
		preview, err := b.Preview(req)
		if err != nil {
			logger.Warn("accum.error", fmt.Sprintf("%s 見積り失敗: %v", po.Symbol, err))
			continue
		}
		totalCost := preview.EstimatedCost.Add(preview.EstimatedFee)
		if totalCost.GreaterThan(buyingPower) {
			logger.Warn("accum.insufficient_funds", fmt.Sprintf("%s: 買付余力不足 (必要 %s / 余力 %s)", po.Symbol, totalCost, buyingPower))
			continue
		}

		mkt := domain.MarketJP
		amt := po.Amount

		if !isLive {
			// dry-run
			_ = led.Record(req, ledger.DryRunStatus, nil, &monthStart, &amt, &mkt)
			logger.Info("accum.dry_run", fmt.Sprintf("[dry-run] %s %s株 @ %s円 (%s)", po.Symbol, req.Quantity, po.LimitPrice, req.Reason))
			continue
		}

		// 実発注
		ack, err := b.Place(req)
		if err != nil {
			logger.Error("accum.order_failed", fmt.Sprintf("%s 発注拒否: %v", po.Symbol, err))
			continue
		}

		_ = led.Record(req, string(ack.Status), ack.BrokerOrderID, &monthStart, &amt, &mkt)
		logger.Info("accum.order", fmt.Sprintf("発注成功: %s %s株 (ID: %s)", po.Symbol, req.Quantity, ack.ClientOrderID))
		buyingPower = buyingPower.Sub(totalCost)
	}

	return nil
}
