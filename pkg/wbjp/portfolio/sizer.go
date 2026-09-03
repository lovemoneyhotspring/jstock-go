package portfolio

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
)

// SizePosition は合成シグナルと資金・価格・ATRから目標建玉数を計算する。
func SizePosition(
	sig domain.CombinedSignal,
	equity decimal.Decimal,
	price decimal.Decimal,
	atr decimal.Decimal,
	lotSize decimal.Decimal,
	cfg wbjpcfg.SizingConfig,
) (domain.TargetPosition, error) {
	if sig.Direction <= 0 {
		return domain.TargetPosition{Symbol: sig.Symbol, Quantity: decimal.Zero, Reason: "シグナルなしまたは売り"}, nil
	}

	var targetQty decimal.Decimal
	var reason string

	switch cfg.Method {
	case "fixed_notional":
		rawQty := cfg.FixedNotional.Div(price).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("fixed_notional (%s円)", cfg.FixedNotional)

	case "equal_weight":
		maxPos := decimal.NewFromInt(int64(cfg.MaxPositions))
		alloc := equity.Div(maxPos)
		rawQty := alloc.Div(price).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("equal_weight (%d銘柄等分, 割当 %s円)", cfg.MaxPositions, alloc.Round(0))

	case "atr_risk":
		fallthrough
	default:
		// 許容損失 = equity * risk_per_trade
		allowedLoss := equity.Mul(cfg.RiskPerTrade)
		// 1株あたり損切り幅 = atr * atr_stop_multiple
		stopWidth := atr.Mul(cfg.ATRStopMultiple)
		if stopWidth.LessThanOrEqual(decimal.Zero) {
			stopWidth = price.Mul(decimal.RequireFromString("0.05")) // フォールバック 5%
		}
		rawQty := allowedLoss.Div(stopWidth).Floor()
		targetQty, _ = marketrules.RoundToLot(rawQty, lotSize)
		reason = fmt.Sprintf("atr_risk (許容損失 %s円 / 損切り幅 %s円)", allowedLoss.Round(0), stopWidth.Round(0))
	}

	return domain.TargetPosition{
		Symbol:   sig.Symbol,
		Quantity: targetQty,
		Reason:   reason,
	}, nil
}
