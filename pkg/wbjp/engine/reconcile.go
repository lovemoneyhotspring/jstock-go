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

// ReconcilePlan は生成した注文と、見送った銘柄の理由。
//
// 見送りを黙って捨てると「なぜ出なかったのか」が後から追えない。
// 差金決済回避や売却可能数量の不足は運用中に実際に起きるため、
// 理由を持ち帰って記録・表示できるようにする。
type ReconcilePlan struct {
	Orders []ReconcileResult
	// Skipped は 銘柄 → 見送りの理由。
	Skipped map[string]string
}

// ReconcileSettings は発注の作り方。
type ReconcileSettings struct {
	OrderType   domain.OrderType
	LimitOffset decimal.Decimal
	TaxType     domain.TaxAccountType
	// Topix500 は呼値の刻みが異なる銘柄の集合。
	Topix500 map[string]struct{}
	// BlocksSameDaySale が真なら当日買付銘柄の売却を止める（現物の差金決済回避）。
	BlocksSameDaySale bool
}

// Reconcile は目標建玉と現在の実効建玉（保有＋未約定残）の差分から注文を作る。
//
// boughtToday は当日買い付けた銘柄。同一資金での「買い→売り→買い」は
// 差金決済にあたるため、現物ではこれらの売却を見送る。
func Reconcile(
	targets map[string]domain.TargetPosition,
	positions map[string]domain.Position,
	openOrders []domain.Order,
	lastPrices map[string]decimal.Decimal,
	lotSizes map[string]decimal.Decimal,
	settings ReconcileSettings,
	boughtToday map[string]struct{},
	today string,
) (*ReconcilePlan, error) {
	if settings.TaxType == "" {
		settings.TaxType = domain.TaxAccountSpecific
	}

	// 実効建玉 = 現有数量 + 未約定残（買はプラス、売はマイナス）
	effectiveHoldings := make(map[string]decimal.Decimal)
	for sym, pos := range positions {
		effectiveHoldings[sym] = pos.Quantity
	}
	for _, o := range openOrders {
		effectiveHoldings[o.Symbol] = effectiveHoldings[o.Symbol].Add(o.SignedRemaining())
	}

	allSymbols := make(map[string]struct{})
	for sym := range targets {
		allSymbols[sym] = struct{}{}
	}
	for sym := range effectiveHoldings {
		allSymbols[sym] = struct{}{}
	}

	plan := &ReconcilePlan{Skipped: make(map[string]string)}

	for _, sym := range sortedKeys(allSymbols) {
		targetQty := decimal.Zero
		reason := ""
		if t, ok := targets[sym]; ok {
			targetQty = t.Quantity
			reason = t.Reason
		}
		currentQty := effectiveHoldings[sym]

		diff := targetQty.Sub(currentQty)
		if diff.IsZero() {
			continue
		}

		lotSize := marketrules.DefaultLotSize
		if l, ok := lotSizes[sym]; ok && l.GreaterThan(decimal.Zero) {
			lotSize = l
		}

		tradable, err := marketrules.RoundToLot(diff.Abs(), lotSize)
		if err != nil {
			plan.Skipped[sym] = err.Error()
			continue
		}
		if tradable.LessThanOrEqual(decimal.Zero) {
			plan.Skipped[sym] = fmt.Sprintf("差分 %s 株が売買単位 %s 株に満たない", diff.Abs(), lotSize)
			continue
		}

		side := domain.SideBuy
		if diff.IsNegative() {
			side = domain.SideSell
		}

		if side == domain.SideSell {
			// 現物の差金決済を避ける（同一資金での同日の買い→売り→買い）。
			if settings.BlocksSameDaySale && marketrules.ViolatesSameDaySettlement(side, sym, boughtToday) {
				plan.Skipped[sym] = "当日買い付けた銘柄のため、差金決済回避で売却を見送り"
				continue
			}

			// 保有を超えて売らない（空売りは不可）。受渡未了分も売れない。
			available := decimal.Zero
			if pos, ok := positions[sym]; ok {
				available = pos.AvailableQuantity
			}
			if tradable.GreaterThan(available) {
				tradable, err = marketrules.RoundToLot(available, lotSize)
				if err != nil || tradable.LessThanOrEqual(decimal.Zero) {
					plan.Skipped[sym] = "売却可能数量が単元株に満たない"
					continue
				}
			}
		}

		lastPrice, hasPrice := lastPrices[sym]
		if settings.OrderType == domain.OrderTypeLimit && (!hasPrice || lastPrice.LessThanOrEqual(decimal.Zero)) {
			plan.Skipped[sym] = "指値の基準となる価格が取得できない"
			continue
		}

		orderType := settings.OrderType
		var limitPrice *decimal.Decimal
		if orderType == domain.OrderTypeLimit {
			// 約定しやすい方向にずらす（買いは上、売りは下）。
			offset := decimal.NewFromInt(1).Add(settings.LimitOffset)
			if side == domain.SideSell {
				offset = decimal.NewFromInt(1).Sub(settings.LimitOffset)
			}
			rawLimit := lastPrice.Mul(offset)

			// 呼値に乗っていない指値は取引所に弾かれるため必ずスナップする。
			// ずらした意図を消さないよう、約定する方向へ丸める。
			_, isTopix500 := settings.Topix500[sym]
			snapped, err := marketrules.SnapToTick(rawLimit, side, isTopix500, marketrules.RoundingAggressive)
			if err != nil {
				plan.Skipped[sym] = fmt.Sprintf("呼値へのスナップに失敗: %v", err)
				continue
			}
			limitPrice = &snapped
		}

		if reason == "" {
			reason = fmt.Sprintf("リコンサイル: 目標 %s株, 現在 %s株 (差分 %s株)", targetQty, currentQty, diff)
		}

		orderID := domain.MakeClientOrderID(today, sym, side, tradable)
		req, err := domain.NewOrderRequest(
			orderID, sym, side, orderType, tradable, limitPrice,
			settings.TaxType, reason, domain.TradeTypeCash,
		)
		if err != nil {
			plan.Skipped[sym] = fmt.Sprintf("注文を組み立てられない: %v", err)
			continue
		}

		plan.Orders = append(plan.Orders, ReconcileResult{
			Symbol:     sym,
			Side:       side,
			Quantity:   tradable,
			LimitPrice: limitPrice,
			Request:    &req,
			Reason:     reason,
		})
	}

	return plan, nil
}

// sortedKeys は map の鍵を並べ替えて返す。注文の順序を実行ごとに揺らさない。
func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
