package plan

import (
	"fmt"
	"math"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

type PlanRow struct {
	Date       string
	Close      decimal.Decimal
	Multiplier float64
	Base       decimal.Decimal
	Extra      decimal.Decimal
	Amount     decimal.Decimal
	Reason     string
}

type AccumPlan struct {
	Rows []PlanRow
}

// BuildPlan は日足とタクティクスから日ごとの投下計画を計算する。
func BuildPlan(bars []domain.Bar, tactic tactics.Tactic, monthlyBudget decimal.Decimal) (*AccumPlan, error) {
	n := len(bars)
	if n == 0 {
		return &AccumPlan{}, nil
	}

	budgetFloat, _ := monthlyBudget.Float64()
	mults := tactic.Multipliers(bars)

	// 月ごとの営業日数を数える (YYYY-MM -> count)
	monthCounts := make(map[string]int)
	// 月の最初の営業日 (YYYY-MM -> first_date)
	firstDayOfMonth := make(map[string]string)

	for _, bar := range bars {
		m := bar.Date[:7]
		monthCounts[m]++
		if _, ok := firstDayOfMonth[m]; !ok {
			firstDayOfMonth[m] = bar.Date
		}
	}

	type dailyAccrual struct {
		date       string
		closePrice decimal.Decimal
		mult       float64
		isPayday   bool
		base       decimal.Decimal
		accrued    int64
		nextWeek   string // YYYY-MM-DD
	}

	daily := make([]dailyAccrual, n)
	for i, bar := range bars {
		m := bar.Date[:7]
		daysInMonth := monthCounts[m]
		isPayday := (firstDayOfMonth[m] == bar.Date)

		base := decimal.Zero
		if isPayday {
			base = monthlyBudget
		}

		accrued := int64(0)
		if mults[i] > 1.0 && daysInMonth > 0 {
			acc := math.Floor((mults[i] - 1.0) * budgetFloat / float64(daysInMonth))
			accrued = int64(acc)
		}

		t, _ := time.Parse("2006-01-02", bar.Date)
		// 翌週の月曜日を計算 (Sunday=0, Monday=1, ..., Saturday=6)
		weekday := int(t.Weekday())
		daysToNextMon := (8 - weekday) % 7
		if daysToNextMon == 0 {
			daysToNextMon = 7
		}
		nextMon := t.AddDate(0, 0, daysToNextMon)
		nextWeekStr := nextMon.Format("2006-01-02")

		daily[i] = dailyAccrual{
			date:       bar.Date,
			closePrice: bar.Close,
			mult:       mults[i],
			isPayday:   isPayday,
			base:       base,
			accrued:    accrued,
			nextWeek:   nextWeekStr,
		}
	}

	// 翌週月曜日ごとにグループ化して合計
	weeklyAccrued := make(map[string]int64)
	weeklyDays := make(map[string]int)
	for _, d := range daily {
		if d.accrued > 0 {
			weeklyAccrued[d.nextWeek] += d.accrued
			weeklyDays[d.nextWeek]++
		}
	}

	// 各 nextWeek に対して、実際に足が存在する最初の営業日にマッピング
	sessionScheduled := make(map[string]int64)
	sessionDays := make(map[string]int)

	for weekStart, accVal := range weeklyAccrued {
		// weekStart 以降で最初の営業日を探す
		for _, bar := range bars {
			if bar.Date >= weekStart {
				sessionScheduled[bar.Date] += accVal
				sessionDays[bar.Date] += weeklyDays[weekStart]
				break
			}
		}
	}

	// 累積が threshold (monthlyBudget) に届いた日、または入金日にリリース
	threshold := int64(budgetFloat)
	releasedExtra := make(map[string]int64)
	releasedDays := make(map[string]int)

	carryAmount := int64(0)
	carryDays := 0

	for _, d := range daily {
		carryAmount += sessionScheduled[d.date]
		carryDays += sessionDays[d.date]

		if carryAmount <= 0 {
			continue
		}

		if carryAmount >= threshold || d.isPayday {
			releasedExtra[d.date] = carryAmount
			releasedDays[d.date] = carryDays
			carryAmount = 0
			carryDays = 0
		}
	}

	// 最終的な PlanRow を作成
	rows := make([]PlanRow, n)
	for i, d := range daily {
		extraVal := releasedExtra[d.date]
		extra := decimal.NewFromInt(extraVal)
		amount := d.base.Add(extra)

		var reason string
		extraD := releasedDays[d.date]
		if d.base.GreaterThan(decimal.Zero) && extra.GreaterThan(decimal.Zero) {
			reason = fmt.Sprintf("入金日 %s 円＋累積の増額 %d 円（下降 %d 日ぶん）", d.base, extraVal, extraD)
		} else if d.base.GreaterThan(decimal.Zero) {
			reason = fmt.Sprintf("入金日 %s 円", d.base)
		} else if extra.GreaterThan(decimal.Zero) {
			reason = fmt.Sprintf("累積の増額 %d 円（下降 %d 日ぶん）", extraVal, extraD)
		} else {
			reason = "投下なし"
		}

		rows[i] = PlanRow{
			Date:       d.date,
			Close:      d.closePrice,
			Multiplier: d.mult,
			Base:       d.base,
			Extra:      extra,
			Amount:     amount,
			Reason:     reason,
		}
	}

	return &AccumPlan{Rows: rows}, nil
}
