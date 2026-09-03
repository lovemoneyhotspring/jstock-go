package risk

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

type Stop struct {
	Symbol           string
	StopPrice        decimal.Decimal
	EntryPrice       decimal.Decimal
	CreatedOn        string
	Trailing         bool
	ATRMultiple      decimal.Decimal
	HighestClose     *decimal.Decimal
	InitialStopPrice *decimal.Decimal
	InitialQuantity  *decimal.Decimal
	ScaledOut        bool
}

func (s *Stop) IsTriggered(price decimal.Decimal) bool {
	return price.LessThanOrEqual(s.StopPrice)
}

type StopBook struct {
	stops map[string]*Stop
}

func NewStopBook(stops map[string]*Stop) *StopBook {
	if stops == nil {
		stops = make(map[string]*Stop)
	}
	return &StopBook{stops: stops}
}

func (sb *StopBook) All() map[string]Stop {
	res := make(map[string]Stop)
	for k, v := range sb.stops {
		res[k] = *v
	}
	return res
}

func (sb *StopBook) Ensure(
	positions map[string]domain.Position,
	atr map[string]decimal.Decimal,
	today string,
	atrMultiple decimal.Decimal,
	trailing bool,
) {
	if atrMultiple.LessThanOrEqual(decimal.Zero) {
		atrMultiple = decimal.RequireFromString("2.0")
	}

	for sym, pos := range positions {
		if pos.Quantity.LessThanOrEqual(decimal.Zero) {
			continue
		}
		if _, exists := sb.stops[sym]; exists {
			continue
		}

		atrVal, hasATR := atr[sym]
		if !hasATR || atrVal.LessThanOrEqual(decimal.Zero) {
			continue
		}

		dist := atrVal.Mul(atrMultiple)
		stopPrice := pos.CostPrice.Sub(dist)
		if stopPrice.LessThanOrEqual(decimal.Zero) {
			continue
		}

		lastP := pos.LastPrice
		initQty := pos.Quantity
		initStop := stopPrice

		sb.stops[sym] = &Stop{
			Symbol:           sym,
			StopPrice:        stopPrice,
			EntryPrice:       pos.CostPrice,
			CreatedOn:        today,
			Trailing:         trailing,
			ATRMultiple:      atrMultiple,
			HighestClose:     &lastP,
			InitialStopPrice: &initStop,
			InitialQuantity:  &initQty,
			ScaledOut:        false,
		}
	}

	// 建玉がなくなった銘柄のストップは削除
	for sym := range sb.stops {
		pos, exists := positions[sym]
		if !exists || pos.Quantity.LessThanOrEqual(decimal.Zero) {
			delete(sb.stops, sym)
		}
	}
}

func (sb *StopBook) UpdateTrailing(closes, atr map[string]decimal.Decimal) {
	for sym, stop := range sb.stops {
		if !stop.Trailing {
			continue
		}

		closePrice, ok := closes[sym]
		if !ok {
			continue
		}

		highest := closePrice
		if stop.HighestClose != nil && stop.HighestClose.GreaterThan(closePrice) {
			highest = *stop.HighestClose
		}
		stop.HighestClose = &highest

		atrVal, hasATR := atr[sym]
		if !hasATR || atrVal.LessThanOrEqual(decimal.Zero) {
			continue
		}

		dist := atrVal.Mul(stop.ATRMultiple)
		candidate := highest.Sub(dist)

		if candidate.GreaterThan(stop.StopPrice) {
			stop.StopPrice = candidate
		}
	}
}

func (sb *StopBook) ExitTargets(closes map[string]decimal.Decimal) []domain.TargetPosition {
	var targets []domain.TargetPosition

	for sym, stop := range sb.stops {
		closePrice, ok := closes[sym]
		if !ok {
			continue
		}

		if stop.IsTriggered(closePrice) {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: decimal.Zero,
				Reason:   fmt.Sprintf("ストップ抵触: 終値 %s <= ストップ %s（全株決済）", closePrice, stop.StopPrice),
			})
		}
	}

	return targets
}

func (sb *StopBook) TimeExitTargets(closes map[string]decimal.Decimal, asOf string, maxDays int) []domain.TargetPosition {
	if maxDays <= 0 {
		return nil
	}

	asOfTime, _ := time.Parse("2006-01-02", asOf)
	var targets []domain.TargetPosition

	for sym, stop := range sb.stops {
		if _, ok := closes[sym]; !ok {
			continue
		}

		createdTime, _ := time.Parse("2006-01-02", stop.CreatedOn)
		diffDays := int(asOfTime.Sub(createdTime).Hours() / 24)

		if diffDays >= maxDays {
			targets = append(targets, domain.TargetPosition{
				Symbol:   sym,
				Quantity: decimal.Zero,
				Reason:   fmt.Sprintf("最大保有期間 %d 日に到達（全株決済）", maxDays),
			})
		}
	}

	return targets
}
