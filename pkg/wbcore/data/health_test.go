package data

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

func healthDay(text string) time.Time {
	d, _ := time.ParseInLocation("2006-01-02", text, time.UTC)
	return d
}

func TestCheckCoverage(t *testing.T) {
	dir := t.TempDir()
	store := NewBarStore(dir)
	if err := store.Write("7203", []domain.Bar{
		bar("7203", "2026-09-01", 100),
		bar("7203", "2026-09-02", 101),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("6758", []domain.Bar{bar("6758", "2026-08-01", 100)}); err != nil {
		t.Fatal(err)
	}

	coverages, err := Check(dir, []string{"7203", "6758", "9999"}, healthDay("2026-09-03"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(coverages) != 3 {
		t.Fatalf("件数 = %d", len(coverages))
	}

	fresh := coverages[0]
	if fresh.Bars != 2 || fresh.First != "2026-09-01" || fresh.Last != "2026-09-02" {
		t.Fatalf("7203 = %+v", fresh)
	}
	if !fresh.Healthy() || fresh.Describe() != "正常" {
		t.Errorf("直近まで取れているのに %s", fresh.Describe())
	}

	stale := coverages[1]
	if !stale.Stale || stale.Healthy() {
		t.Errorf("1 か月前で止まっているのに健全扱い: %+v", stale)
	}
	if stale.Describe() != "最終 2026-08-01 で止まっている" {
		t.Errorf("Describe = %s", stale.Describe())
	}

	missing := coverages[2]
	if missing.Bars != 0 || missing.Healthy() || missing.Describe() != "足が無い" {
		t.Errorf("未保存の銘柄 = %+v", missing)
	}
}

// 週末を挟んでも「止まっている」と誤判定しないこと（カレンダー無し＝平日で代用）。
func TestStaleThresholdAllowsWeekend(t *testing.T) {
	// 金曜の足を、翌週の火曜に見る（抜けは月曜の 1 営業日＝閾値ちょうどなので止まっていない）
	if isStale("2026-08-28", healthDay("2026-09-01"), nil) {
		t.Error("週末を挟んだだけで止まっている扱いになっている")
	}
	// 水曜に見ると月・火の 2 営業日が抜けている
	if !isStale("2026-08-28", healthDay("2026-09-02"), nil) {
		t.Error("2 営業日抜けた足は止まっている扱いにすべき")
	}
}

// 連休は営業日に数えないこと。2026-09-18（金）の足を、5 連休（9/19〜23）の最終日に見ても誤報にしない。
func TestStaleSkipsHolidays(t *testing.T) {
	holidays := map[string]bool{"2026-09-21": true, "2026-09-22": true, "2026-09-23": true}
	isTradingDay := func(day time.Time) bool {
		return isWeekday(day) && !holidays[day.Format("2006-01-02")]
	}
	if isStale("2026-09-18", healthDay("2026-09-23"), isTradingDay) {
		t.Error("連休の最終日に止まっている扱いになっている")
	}
	// 連休明けの 9/24 の足が取れず、9/25 に見る（抜けは 9/24 の 1 営業日）
	if isStale("2026-09-18", healthDay("2026-09-25"), isTradingDay) {
		t.Error("連休明けの 1 営業日の抜けで止まっている扱いになっている")
	}
	// 9/28（月）に見ると 9/24・25 の 2 営業日が抜けている
	if !isStale("2026-09-18", healthDay("2026-09-28"), isTradingDay) {
		t.Error("連休明けに 2 営業日抜けているのに健全扱い")
	}
	// カレンダーが無いと 9/21・22 を営業日に数えて止まっている扱いになる（従来の誤報）
	if !isStale("2026-09-18", healthDay("2026-09-23"), nil) {
		t.Error("平日で代用したときの判定が変わっている")
	}
}

func TestStaleUnreadableDate(t *testing.T) {
	if !isStale("not-a-date", healthDay("2026-09-23"), nil) {
		t.Error("日付として読めない足は止まっている扱いにすべき")
	}
}
