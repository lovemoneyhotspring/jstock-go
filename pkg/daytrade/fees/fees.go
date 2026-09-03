// Package fees は日本株（現物）の手数料と、資金から銘柄数を決める規則。
//
// 手数料は立花証券の**定額コース**（broker.FlatRateTable）。1 日の現物約定代金の
// **合計**で段階が決まり、12 万円まで 0 円、20 万円まで 176 円、50 万円まで 253 円、
// 100 万円まで 506 円、以後 100 万円ごとに 253 円。信用取引は手数料 0 円で、
// こちらは backtest が commission=false で扱う。
//
// 1 日の合計で決まるので「1 注文をいくらにするか」で bp は大きくは変わらないが、
// 合計が段階の境目を越えるかどうかで変わる。**銘柄数は資金を 1 注文の目安額で割って
// 決める**規則（PositionsFor）はそのまま。
package fees

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/shopspring/decimal"
)

// Commission はその日の現物約定代金の合計 dayTotal に対する手数料（1 日分の総額）。0 以下は 0。
func Commission(dayTotal decimal.Decimal) decimal.Decimal {
	return broker.FlatRateCommission(dayTotal)
}

// OrderFeeEstimate は 1 注文の片道手数料の見込み。
// その注文だけを往復した日として（合計 = 2 × 代金）見積もる。
func OrderFeeEstimate(amount decimal.Decimal) decimal.Decimal {
	if amount.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return Commission(amount.Mul(decimal.NewFromInt(2))).Div(decimal.NewFromInt(2))
}

// RoundTripBP は 1 注文だけを往復したときの手数料を bp で。約定代金が 0 以下なら 0。
func RoundTripBP(amount decimal.Decimal) decimal.Decimal {
	if amount.LessThanOrEqual(decimal.Zero) {
		return decimal.Zero
	}
	return Commission(amount.Mul(decimal.NewFromInt(2))).Div(amount).Mul(decimal.NewFromInt(10_000))
}

// PositionsFor は資金から銘柄数 N を決める。
//
// maxCapital ÷ orderBudget を四捨五入し、1 以上 maxPositions 以下に収める
// （200 万 ÷ 67 万 = 2.98 → 3）。1 注文あたりの予算は呼び出し側で maxCapital ÷ N に
// する（余りを均等に配る）。
//
// 資金が 1 注文の目安の半分に届かなければエラー——「N をいくつにするか」を人が
// 書かない代わりに、資金と目安が矛盾していることをここで気づかせる。
func PositionsFor(maxCapital, orderBudget decimal.Decimal, maxPositions int) (int, error) {
	if maxCapital.LessThanOrEqual(decimal.Zero) || orderBudget.LessThanOrEqual(decimal.Zero) {
		return 0, fmt.Errorf("max_capital と order_budget は正の値")
	}
	if maxPositions < 1 {
		return 0, fmt.Errorf("max_positions は 1 以上")
	}
	n := int(maxCapital.Div(orderBudget).Round(0).IntPart())
	if n < 1 {
		return 0, fmt.Errorf(
			"資金 %s 円が 1 注文の目安 %s 円の半分に届きません。order_budget を下げるか資金を増やしてください",
			maxCapital.StringFixed(0), orderBudget.StringFixed(0))
	}
	return min(n, maxPositions), nil
}
