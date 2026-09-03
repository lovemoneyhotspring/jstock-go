package engine

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/shopspring/decimal"
)

// ReconcileResult はリコンサイルで生成された注文リクエスト。
type ReconcileResult struct {
	Symbol     string
	Side       domain.Side
	Quantity   decimal.Decimal
	LimitPrice *decimal.Decimal
	Request    *domain.OrderRequest
	Reason     string
}

// Reconcile は目標建玉と現在の実効建玉（保有＋未約定残）の差分を計算し、必要な注文を生成する。
func Reconcile(
	targets map[string]domain.TargetPosition,
	positions map[string]domain.Position,
	openOrders []domain.Order,
	lastPrices map[string]decimal.Decimal,
	lotSizes map[string]decimal.Decimal,
	orderType domain.OrderType,
	limitOffset decimal.Decimal,
	today string,
) ([]ReconcileResult, error) {
	// 実効建玉 = 現有数量 + 未約定残（買はプラス、売はマイナス）
	effectiveHoldings := make(map[string]decimal.Decimal)
	for sym, pos := range positions {
		effectiveHoldings[sym] = pos.Quantity
	}
	for _, o := range openOrders {
		current := effectiveHoldings[o.Symbol]
		effectiveHoldings[o.Symbol] = current.Add(o.SignedRemaining())
	}

	// 対象銘柄の集合
	allSymbols := make(map[string]struct{})
	for sym := range targets {
		allSymbols[sym] = struct{}{}
	}
	for sym := range effectiveHoldings {
		allSymbols[sym] = struct{}{}
	}

	var results []ReconcileResult

	for sym := range allSymbols {
		targetQty := decimal.Zero
		if t, ok := targets[sym]; ok {
			targetQty = t.Quantity
		}
		currentQty := effectiveHoldings[sym]

		diff := targetQty.Sub(currentQty)
		lotSize := marketrules.DefaultLotSize
		if l, ok := lotSizes[sym]; ok && l.GreaterThan(decimal.Zero) {
			lotSize = l
		}

		// 単元株数に丸め
		roundedDiff, _ := marketrules.RoundToLot(diff.Abs(), lotSize)
		if roundedDiff.LessThanOrEqual(decimal.Zero) {
			continue // 差分なし、または単元未満
		}

		side := domain.SideBuy
		if diff.IsNegative() {
			side = domain.SideSell
		}

		lastPrice, ok := lastPrices[sym]
		if !ok || lastPrice.LessThanOrEqual(decimal.Zero) {
			continue
		}

		var limitPrice *decimal.Decimal
		if orderType == domain.OrderTypeLimit {
			direction := decimal.NewFromInt(1)
			if side == domain.SideSell {
				direction = decimal.NewFromInt(-1)
			}
			rawLimit := lastPrice.Mul(decimal.NewFromInt(1).Add(direction.Mul(limitOffset)))
			snapped, _ := marketrules.SnapToTick(rawLimit, side, false, marketrules.RoundingConservative)
			limitPrice = &snapped
		}

		orderID := domain.MakeClientOrderID(today, sym, side, roundedDiff)
		reason := fmt.Sprintf("リコンサイル: 目標 %s株, 現在 %s株 (差分 %s株)", targetQty, currentQty, diff)

		req, err := domain.NewOrderRequest(
			orderID,
			sym,
			side,
			orderType,
			roundedDiff,
			limitPrice,
			domain.TaxAccountSpecific,
			reason,
			domain.TradeTypeCash,
		)
		if err != nil {
			continue
		}

		results = append(results, ReconcileResult{
			Symbol:     sym,
			Side:       side,
			Quantity:   roundedDiff,
			LimitPrice: limitPrice,
			Request:    &req,
			Reason:     reason,
		})
	}

	return results, nil
}
