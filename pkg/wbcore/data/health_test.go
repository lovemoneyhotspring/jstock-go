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

	coverages, err := Check(dir, []string{"7203", "6758", "9999"}, healthDay("2026-09-03"))
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

// 週末を挟んでも「止まっている」と誤判定しないこと。
func TestStaleThresholdAllowsWeekend(t *testing.T) {
	// 金曜の足を、翌週の火曜に見る（4 日差＝閾値ちょうどなので止まっていない）
	if isStale("2026-08-28", healthDay("2026-09-01")) {
		t.Error("週末を挟んだだけで止まっている扱いになっている")
	}
	if !isStale("2026-08-28", healthDay("2026-09-02")) {
		t.Error("5 日前の足は止まっている扱いにすべき")
	}
}
