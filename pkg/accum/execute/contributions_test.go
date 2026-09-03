package execute

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/shopspring/decimal"
)

func month(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr(t time.Time) *time.Time { return &t }

// row は日付・基本・増額だけを持つ計画行。
func row(date string, base, extra int64) plan.PlanRow {
	return plan.PlanRow{
		Date:   date,
		Base:   decimal.NewFromInt(base),
		Extra:  decimal.NewFromInt(extra),
		Amount: decimal.NewFromInt(base + extra),
	}
}

func TestMonthTargetFullMonth(t *testing.T) {
	rows := []plan.PlanRow{
		row("2026-09-01", 25000, 0),
		row("2026-09-02", 0, 0),
	}
	base, extras, prorated := MonthTarget(rows, decimal.NewFromInt(25000), month("2026-09-01"), nil)

	if !base.Equal(decimal.NewFromInt(25000)) {
		t.Errorf("基本 = %s, want 25000", base)
	}
	if !extras.IsZero() {
		t.Errorf("増額 = %s, want 0", extras)
	}
	if prorated != "" {
		t.Errorf("日割りしないはず: %q", prorated)
	}
}

func TestMonthTargetBeforePayday(t *testing.T) {
	// 入金日の足がまだ確定していなければ基本は 0。
	rows := []plan.PlanRow{row("2026-09-01", 0, 0)}
	base, _, _ := MonthTarget(rows, decimal.NewFromInt(25000), month("2026-09-01"), nil)
	if !base.IsZero() {
		t.Errorf("入金日前の基本 = %s, want 0", base)
	}
}

func TestMonthTargetProratesStartMonth(t *testing.T) {
	// 9/16 開始 → 残り 15/30 日 → 25000 × 15/30 = 12500。
	rows := []plan.PlanRow{
		row("2026-09-01", 25000, 0),
		row("2026-09-16", 0, 0),
	}
	started := month("2026-09-16")
	base, _, prorated := MonthTarget(rows, decimal.NewFromInt(25000), month("2026-09-01"), &started)

	if !base.Equal(decimal.NewFromInt(12500)) {
		t.Errorf("日割り後の基本 = %s, want 12500", base)
	}
	if prorated == "" {
		t.Error("日割りの説明を返すべき")
	}
}

func TestMonthTargetIgnoresExtrasBeforeStart(t *testing.T) {
	// 開始日より前に積み上がった増額は数えない。
	rows := []plan.PlanRow{
		row("2026-09-01", 25000, 0),
		row("2026-09-10", 0, 50000), // 開始前の増額
		row("2026-09-20", 0, 30000), // 開始後の増額
	}
	started := month("2026-09-16")
	_, extras, _ := MonthTarget(rows, decimal.NewFromInt(25000), month("2026-09-01"), &started)

	if !extras.Equal(decimal.NewFromInt(30000)) {
		t.Errorf("増額 = %s, want 30000（開始前の 50000 は数えない）", extras)
	}
}

func TestMonthTargetFullFromSecondMonth(t *testing.T) {
	// 開始月の翌月からは日割りしない。
	rows := []plan.PlanRow{row("2026-10-01", 25000, 0)}
	started := month("2026-09-16")
	base, _, prorated := MonthTarget(rows, decimal.NewFromInt(25000), month("2026-10-01"), &started)

	if !base.Equal(decimal.NewFromInt(25000)) {
		t.Errorf("翌月の基本 = %s, want 25000", base)
	}
	if prorated != "" {
		t.Errorf("翌月は日割りしない: %q", prorated)
	}
}

func TestDaysInMonth(t *testing.T) {
	cases := map[string]int{
		"2026-09-01": 30,
		"2026-10-01": 31,
		"2026-02-01": 28,
		"2024-02-01": 29, // 閏年
	}
	for m, want := range cases {
		if got := daysInMonth(month(m)); got != want {
			t.Errorf("daysInMonth(%s) = %d, want %d", m, got, want)
		}
	}
}

func TestCarryOverRequiresPriorOrders(t *testing.T) {
	rows := []plan.PlanRow{row("2026-08-01", 25000, 0), row("2026-09-01", 25000, 0)}
	budget := decimal.NewFromInt(25000)

	noOrders := func(string, time.Time) bool { return false }
	placed := func(string, time.Time) decimal.Decimal { return decimal.Zero }

	// 前月に発注が無ければ繰り越さない（動いていなかった月のぶんまで買わない）。
	got := CarryOver(rows, "1306", month("2026-09-01"), budget, nil, noOrders, placed)
	if !got.IsZero() {
		t.Errorf("前月に発注が無ければ 0: %s", got)
	}
}

func TestCarryOverRemainder(t *testing.T) {
	rows := []plan.PlanRow{row("2026-08-01", 25000, 0), row("2026-09-01", 25000, 0)}
	budget := decimal.NewFromInt(25000)

	hadOrders := func(string, time.Time) bool { return true }
	// 前月は 20000 円しか買えなかった（単元に届かず端数が残った）。
	placed := func(_ string, m time.Time) decimal.Decimal {
		if m.Month() == time.August {
			return decimal.NewFromInt(20000)
		}
		return decimal.Zero
	}

	got := CarryOver(rows, "1306", month("2026-09-01"), budget, nil, hadOrders, placed)
	if !got.Equal(decimal.NewFromInt(5000)) {
		t.Errorf("繰り越し = %s, want 5000", got)
	}
}

func TestCarryOverNeverNegative(t *testing.T) {
	rows := []plan.PlanRow{row("2026-08-01", 25000, 0)}
	hadOrders := func(string, time.Time) bool { return true }
	// 前月に多く買っていても、当月にマイナスを持ち込まない。
	placed := func(string, time.Time) decimal.Decimal { return decimal.NewFromInt(40000) }

	got := CarryOver(rows, "1306", month("2026-09-01"), decimal.NewFromInt(25000), nil, hadOrders, placed)
	if !got.IsZero() {
		t.Errorf("買い過ぎた月の繰り越しは 0: %s", got)
	}
}

func TestCarryOverUsesProratedTargetForStartMonth(t *testing.T) {
	// 前月が開始月なら、日割り後の額を目標にする。満額で計算すると
	// 日割りで買わなかった分まで買い残しとして当月に上乗せされる。
	rows := []plan.PlanRow{row("2026-08-01", 25000, 0), row("2026-09-01", 25000, 0)}
	started := month("2026-08-16") // 8月は31日 → 残り16/31 → 12903

	hadOrders := func(string, time.Time) bool { return true }
	placed := func(_ string, m time.Time) decimal.Decimal {
		if m.Month() == time.August {
			return decimal.NewFromInt(12903)
		}
		return decimal.Zero
	}

	got := CarryOver(rows, "1306", month("2026-09-01"), decimal.NewFromInt(25000), &started, hadOrders, placed)
	if !got.IsZero() {
		t.Errorf("日割りぶんを買い切っていれば繰り越しは 0: %s", got)
	}
}

func TestShouldPlaceToday(t *testing.T) {
	base := decimal.NewFromInt(25000)

	releaseDay := []plan.PlanRow{row("2026-09-01", 25000, 0)}
	quietDay := []plan.PlanRow{row("2026-09-02", 0, 0)}

	cases := []struct {
		name string
		rows []plan.PlanRow
		due  decimal.Decimal
		want bool
	}{
		{"リリース日なら小額でも出す", releaseDay, decimal.NewFromInt(100), true},
		{"平常日の端数は持ち越す", quietDay, decimal.NewFromInt(100), false},
		{"平常日でも基本目標以上なら出す", quietDay, decimal.NewFromInt(25000), true},
		{"平常日で基本目標未満は持ち越す", quietDay, decimal.NewFromInt(24999), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldPlaceToday(tc.rows, tc.due, base); got != tc.want {
				t.Errorf("ShouldPlaceToday = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldPlaceTodayBeforePayday(t *testing.T) {
	// 入金日前（基本目標 0）は、リリース日でなければ出さない。
	quietDay := []plan.PlanRow{row("2026-09-02", 0, 0)}
	if ShouldPlaceToday(quietDay, decimal.NewFromInt(1000), decimal.Zero) {
		t.Error("基本目標が 0 のときに端数を出してはいけない")
	}
}
