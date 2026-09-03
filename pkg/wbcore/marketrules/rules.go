package marketrules

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

var (
	DefaultLotSize = decimal.NewFromInt(100)

	d1000    = decimal.NewFromInt(1000)
	d3000    = decimal.NewFromInt(3000)
	d5000    = decimal.NewFromInt(5000)
	d10000   = decimal.NewFromInt(10000)
	d30000   = decimal.NewFromInt(30000)
	d50000   = decimal.NewFromInt(50000)
	d100000  = decimal.NewFromInt(100000)
	d300000  = decimal.NewFromInt(300000)
	d500000  = decimal.NewFromInt(500000)
	d1000000 = decimal.NewFromInt(1000000)
	d3000000 = decimal.NewFromInt(3000000)
	d5000000 = decimal.NewFromInt(5000000)
	d10000000 = decimal.NewFromInt(10000000)
	d30000000 = decimal.NewFromInt(30000000)
	d50000000 = decimal.NewFromInt(50000000)

	d0_1 = decimal.RequireFromString("0.1")
	d0_5 = decimal.RequireFromString("0.5")
	d1   = decimal.NewFromInt(1)
	d5   = decimal.NewFromInt(5)
	d10  = decimal.NewFromInt(10)
	d50  = decimal.NewFromInt(50)
	d100 = decimal.NewFromInt(100)
	d500 = decimal.NewFromInt(500)
	d1000Val = decimal.NewFromInt(1000)
	d5000Val = decimal.NewFromInt(5000)
	d10000Val = decimal.NewFromInt(10000)
	d50000Val = decimal.NewFromInt(50000)
	d100000Val = decimal.NewFromInt(100000)

	// 通常銘柄の呼値テーブル (上限「以下」区分)
	tickStandard = []struct {
		bound decimal.Decimal
		unit  decimal.Decimal
	}{
		{d1000, d1},
		{d3000, d1},
		{d5000, d5},
		{d10000, d10},
		{d30000, d10},
		{d50000, d50},
		{d100000, d100},
		{d300000, d100},
		{d500000, d500},
		{d1000000, d1000Val},
		{d3000000, d1000Val},
		{d5000000, d5000Val},
		{d10000000, d10000Val},
		{d30000000, d10000Val},
		{d50000000, d50000Val},
	}
	tickStandardAbove = d100000Val

	// TOPIX500 構成銘柄の呼値テーブル (上限「以下」区分)
	tickTOPIX500 = []struct {
		bound decimal.Decimal
		unit  decimal.Decimal
	}{
		{d1000, d0_1},
		{d3000, d0_5},
		{d5000, d1},
		{d10000, d1},
		{d30000, d5},
		{d50000, d10},
		{d100000, d10},
		{d300000, d50},
		{d500000, d100},
		{d1000000, d100},
		{d3000000, d500},
		{d5000000, d1000Val},
		{d10000000, d1000Val},
		{d30000000, d5000Val},
		{d50000000, d10000Val},
	}
	tickTOPIX500Above = d10000Val
)

// TickSize は指定した価格帯における呼値の単位を返す。
// 呼値テーブルは「この値以下」で判定。
func TickSize(price decimal.Decimal, topix500 bool) (decimal.Decimal, error) {
	if price.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("price は正の数: %s", price)
	}

	table := tickStandard
	above := tickStandardAbove
	if topix500 {
		table = tickTOPIX500
		above = tickTOPIX500Above
	}

	for _, entry := range table {
		if price.LessThanOrEqual(entry.bound) {
			return entry.unit, nil
		}
	}
	return above, nil
}

type PriceRounding int

const (
	RoundingConservative PriceRounding = iota // 買いは切下げ、売りは切上げ
	RoundingAggressive                        // 買いは切上げ、売りは切下げ
	RoundingNearest                           // 四捨五入
)

// SnapToTick は指定価格を有効な呼値に乗せる。
func SnapToTick(price decimal.Decimal, side domain.Side, topix500 bool, rounding PriceRounding) (decimal.Decimal, error) {
	if price.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("price は正の数: %s", price)
	}

	current := price
	for i := 0; i < 4; i++ {
		tick, err := TickSize(current, topix500)
		if err != nil {
			return decimal.Zero, err
		}

		// current / tick
		div := current.Div(tick)
		var snappedUnits decimal.Decimal

		switch rounding {
		case RoundingNearest:
			snappedUnits = div.Round(0)
		case RoundingConservative:
			if side == domain.SideBuy {
				snappedUnits = div.Floor()
			} else {
				snappedUnits = div.Ceil()
			}
		case RoundingAggressive:
			if side == domain.SideBuy {
				snappedUnits = div.Ceil()
			} else {
				snappedUnits = div.Floor()
			}
		}

		snapped := snappedUnits.Mul(tick)
		if snapped.LessThanOrEqual(decimal.Zero) {
			snapped = tick
		}

		nextTick, _ := TickSize(snapped, topix500)
		if snapped.Equal(current) || nextTick.Equal(tick) {
			return snapped, nil
		}
		current = snapped
	}
	return current, nil
}

// 制限値幅テーブル (基準値段の上限「未満」区分)
var priceLimits = []struct {
	bound decimal.Decimal
	width decimal.Decimal
}{
	{decimal.NewFromInt(100), decimal.NewFromInt(30)},
	{decimal.NewFromInt(200), decimal.NewFromInt(50)},
	{decimal.NewFromInt(500), decimal.NewFromInt(80)},
	{decimal.NewFromInt(700), decimal.NewFromInt(100)},
	{decimal.NewFromInt(1000), decimal.NewFromInt(150)},
	{decimal.NewFromInt(1500), decimal.NewFromInt(300)},
	{decimal.NewFromInt(2000), decimal.NewFromInt(400)},
	{decimal.NewFromInt(3000), decimal.NewFromInt(500)},
	{decimal.NewFromInt(5000), decimal.NewFromInt(700)},
	{decimal.NewFromInt(7000), decimal.NewFromInt(1000)},
	{decimal.NewFromInt(10000), decimal.NewFromInt(1500)},
	{decimal.NewFromInt(15000), decimal.NewFromInt(3000)},
	{decimal.NewFromInt(20000), decimal.NewFromInt(4000)},
	{decimal.NewFromInt(30000), decimal.NewFromInt(5000)},
	{decimal.NewFromInt(50000), decimal.NewFromInt(7000)},
	{decimal.NewFromInt(70000), decimal.NewFromInt(10000)},
	{decimal.NewFromInt(100000), decimal.NewFromInt(15000)},
	{decimal.NewFromInt(150000), decimal.NewFromInt(30000)},
	{decimal.NewFromInt(200000), decimal.NewFromInt(40000)},
	{decimal.NewFromInt(300000), decimal.NewFromInt(50000)},
	{decimal.NewFromInt(500000), decimal.NewFromInt(70000)},
	{decimal.NewFromInt(700000), decimal.NewFromInt(100000)},
	{decimal.NewFromInt(1000000), decimal.NewFromInt(150000)},
	{decimal.NewFromInt(1500000), decimal.NewFromInt(300000)},
	{decimal.NewFromInt(2000000), decimal.NewFromInt(400000)},
	{decimal.NewFromInt(3000000), decimal.NewFromInt(500000)},
	{decimal.NewFromInt(5000000), decimal.NewFromInt(700000)},
	{decimal.NewFromInt(7000000), decimal.NewFromInt(1000000)},
	{decimal.NewFromInt(10000000), decimal.NewFromInt(1500000)},
	{decimal.NewFromInt(15000000), decimal.NewFromInt(3000000)},
	{decimal.NewFromInt(20000000), decimal.NewFromInt(4000000)},
	{decimal.NewFromInt(30000000), decimal.NewFromInt(5000000)},
	{decimal.NewFromInt(50000000), decimal.NewFromInt(7000000)},
}
var priceLimitAbove = decimal.NewFromInt(10000000)

// PriceLimitWidth は基準値段に対する制限値幅（上下片側）を返す。
// 「未満」区分なので、境界値ちょうどは上の区分に入る。
func PriceLimitWidth(basePrice decimal.Decimal) (decimal.Decimal, error) {
	if basePrice.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("base_price は正の数: %s", basePrice)
	}

	for _, entry := range priceLimits {
		if basePrice.LessThan(entry.bound) {
			return entry.width, nil
		}
	}
	return priceLimitAbove, nil
}

// PriceLimitRange はストップ安とストップ高のペア (low, high) を返す。
func PriceLimitRange(basePrice decimal.Decimal) (decimal.Decimal, decimal.Decimal, error) {
	width, err := PriceLimitWidth(basePrice)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	low := basePrice.Sub(width)
	if low.LessThan(d1) {
		low = d1
	}
	high := basePrice.Add(width)
	return low, high, nil
}

// IsWithinPriceLimit は指値が制限値幅の内側にあるかを判定する。
func IsWithinPriceLimit(price, basePrice decimal.Decimal) (bool, error) {
	low, high, err := PriceLimitRange(basePrice)
	if err != nil {
		return false, err
	}
	return price.GreaterThanOrEqual(low) && price.LessThanOrEqual(high), nil
}

// RoundToLot は数量を単元株数に切り捨てる。
func RoundToLot(quantity decimal.Decimal, lotSize decimal.Decimal) (decimal.Decimal, error) {
	if lotSize.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero, fmt.Errorf("lot_size は正の数: %s", lotSize)
	}
	if quantity.IsNegative() {
		neg := quantity.Neg()
		floored := neg.Div(lotSize).Floor().Mul(lotSize)
		return floored.Neg(), nil
	}
	return quantity.Div(lotSize).Floor().Mul(lotSize), nil
}

// 取引時間（JST）
var (
	morningOpen    = 9*60 + 0
	morningClose   = 11*60 + 30
	afternoonOpen  = 12*60 + 30
	afternoonClose = 15*60 + 30
)

// IsTradingHours は指定日時が東証の立会時間内（ザラ場中）かを判定する。
func IsTradingHours(when time.Time) bool {
	jst := clock.ToZone(when, clock.Tokyo)
	weekday := jst.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return false
	}
	minuteOfDay := jst.Hour()*60 + jst.Minute()
	isMorning := minuteOfDay >= morningOpen && minuteOfDay <= morningClose
	isAfternoon := minuteOfDay >= afternoonOpen && minuteOfDay <= afternoonClose
	return isMorning || isAfternoon
}

// ViolatesSameDaySettlement は当日買い付けた銘柄を当日売却する差金決済（日計り）違反を未然に防止する。
func ViolatesSameDaySettlement(side domain.Side, symbol string, boughtToday map[string]struct{}) bool {
	if side != domain.SideSell {
		return false
	}
	_, exists := boughtToday[symbol]
	return exists
}
