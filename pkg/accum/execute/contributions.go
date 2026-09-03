package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/shopspring/decimal"
)

// MonthTarget は今月の目標を（基本, 増額, 日割りの説明）で返す。
//
// 基本は入金日の足が確定していれば予算の全額。開始日が今月の途中なら
// 残り暦日数で日割りする（開始日を含む）。増額は開始日以降に出す分だけ。
//
// 日割りが無いと、月の途中で積立を始めた銘柄にその月の満額を投じてしまう。
func MonthTarget(
	thisMonth []plan.PlanRow,
	budget decimal.Decimal,
	month time.Time,
	started *time.Time,
) (base, extras decimal.Decimal, prorated string) {
	// 入金日（基本予算が立つ日）の足が確定しているか。
	paydayPassed := false
	for _, r := range thisMonth {
		if r.Base.IsPositive() {
			paydayPassed = true
			break
		}
	}

	inStartMonth := started != nil &&
		started.Year() == month.Year() && started.Month() == month.Month() &&
		started.Day() > 1

	// 開始月なら、開始日より前に積み上がった増額は数えない。
	extras = decimal.Zero
	for _, r := range thisMonth {
		if inStartMonth && r.Date < started.Format("2006-01-02") {
			continue
		}
		extras = extras.Add(r.Extra)
	}

	if !paydayPassed {
		return decimal.Zero, extras, ""
	}
	if !inStartMonth {
		return budget.Truncate(0), extras, ""
	}

	days := daysInMonth(month)
	remaining := days - started.Day() + 1
	base = budget.Mul(decimal.NewFromInt(int64(remaining))).
		Div(decimal.NewFromInt(int64(days))).Truncate(0)
	return base, extras, fmt.Sprintf("%d/%d 開始、残り %d/%d 日で日割り",
		int(started.Month()), started.Day(), remaining, days)
}

// daysInMonth はその月の暦日数。
func daysInMonth(month time.Time) int {
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	return int(first.AddDate(0, 1, 0).Sub(first).Hours() / 24)
}

// CarryOver は前月の「目標 − 発注済み」の残り。前月に発注記録が無ければ 0。
//
// 単元に届かず買えなかった端数や、月末に積み上がった増額はここに残る。
// 前月に実際の発注があったときだけ繰り越す（動いていなかった月や
// dry-run だけの月のぶんまで買わないため）。
//
// 前月の目標は当月と同じ規則で出す。前月が開始月なら日割り後の額が目標。
// 満額で計算すると、日割りで買わなかった分まで「買い残し」として
// 当月に上乗せされ、二重買付になる。
func CarryOver(
	rows []plan.PlanRow,
	symbol string,
	month time.Time,
	budget decimal.Decimal,
	started *time.Time,
	hadOrders func(symbol string, month time.Time) bool,
	placedAmount func(symbol string, month time.Time) decimal.Decimal,
) decimal.Decimal {
	previous := month.AddDate(0, -1, 0)
	if !hadOrders(symbol, previous) {
		return decimal.Zero
	}

	prevKey := previous.Format("2006-01")
	var lastMonth []plan.PlanRow
	for _, r := range rows {
		if len(r.Date) >= 7 && r.Date[:7] == prevKey {
			lastMonth = append(lastMonth, r)
		}
	}
	if len(lastMonth) == 0 {
		return decimal.Zero
	}

	base, extras, _ := MonthTarget(lastMonth, budget, previous, started)
	remaining := base.Add(extras).Sub(placedAmount(symbol, previous))
	if remaining.IsNegative() {
		return decimal.Zero
	}
	return remaining
}

// IsReleaseDay は直前の確定足が入金日か増額のリリース日か。
//
// その日はどのみち注文が出るので、端数の差額を同じ注文に乗せられる。
func IsReleaseDay(thisMonth []plan.PlanRow) bool {
	if len(thisMonth) == 0 {
		return false
	}
	return thisMonth[len(thisMonth)-1].Amount.IsPositive()
}

// ShouldPlaceToday は差額を今日出すか、次のリリース日まで持ち越すかを決める。
//
// 単元に丸めて買えなかった端数や小さな予算増をそのまま出すと、株価が
// 下がった日に 1 単元だけの小口注文になる。手数料をまとめるため、
// 差額は次のどちらかのときだけ出す。
//
//   - 直前の確定足がリリース日（どのみち注文が出る日）
//   - 差額が今月の基本目標以上（入金日の注文が丸ごと通らなかった、
//     cron が止まっていた、月の途中で始めた、など）
func ShouldPlaceToday(thisMonth []plan.PlanRow, due, baseTarget decimal.Decimal) bool {
	if IsReleaseDay(thisMonth) {
		return true
	}
	return baseTarget.IsPositive() && due.GreaterThanOrEqual(baseTarget)
}
