package repo

import (
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// TestRealizedPnLOn は W5 の材料。その日に出して約定した売りの実現損益を、売る前の建玉の
// 記録（取得単価）から出す。取得単価・約定単価が分からない売りは Unpriced に残す。
func TestRealizedPnLOn(t *testing.T) {
	r := openTemp(t)
	startRun(t, r, "run-1")
	today := dayJST(clock.NowUTC())

	// 売る前の建玉の記録（取得単価 1000 円）
	if err := r.RecordSnapshot("run-1", today, []domain.Position{
		{Symbol: "7203", Quantity: dec("200"), CostPrice: dec("1000"), LastPrice: dec("950")},
	}); err != nil {
		t.Fatal(err)
	}
	// 100 株を 900 円で約定 → −10,000 円
	sell := newRequest(t, "s1", "7203", domain.SideSell, 100)
	id := "1/20260924"
	if err := r.RecordOrder("run-1", sell, string(domain.OrderStatusSubmitted), &id); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateOrder("s1", domain.OrderStatusFilled, dec("100"), ptr(dec("900")), &id); err != nil {
		t.Fatal(err)
	}
	// 約定していない売り・買い・dry-run は数えない
	for _, rec := range []struct {
		req    domain.OrderRequest
		status string
	}{
		{newRequest(t, "s2", "7203", domain.SideSell, 100), string(domain.OrderStatusSubmitted)},
		{newRequest(t, "b1", "6758", domain.SideBuy, 100), string(domain.OrderStatusSubmitted)},
		{newRequest(t, "d1", "7203", domain.SideSell, 100), "dry_run"},
	} {
		if err := r.RecordOrder("run-1", rec.req, rec.status, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.UpdateOrder("b1", domain.OrderStatusFilled, dec("100"), ptr(dec("500")), nil); err != nil {
		t.Fatal(err)
	}

	got, err := r.RealizedPnLOn(today)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Amount.Equal(dec("-10000")) || len(got.Unpriced) != 0 {
		t.Errorf("実現損益: %+v", got)
	}

	// 建玉の記録が無い銘柄の売り → 取得単価が分からない
	orphan := newRequest(t, "s3", "9984", domain.SideSell, 100)
	if err := r.RecordOrder("run-1", orphan, string(domain.OrderStatusSubmitted), nil); err != nil {
		t.Fatal(err)
	}
	if err := r.UpdateOrder("s3", domain.OrderStatusFilled, dec("100"), ptr(dec("900")), nil); err != nil {
		t.Fatal(err)
	}
	got, err = r.RealizedPnLOn(today)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unpriced) != 1 || !strings.Contains(got.Unpriced[0], "9984") {
		t.Errorf("取得単価の分からない売りが Unpriced に無い: %+v", got)
	}

	// 別の日は数えない
	if other, err := r.RealizedPnLOn("2000-01-01"); err != nil || !other.Amount.IsZero() || len(other.Unpriced) != 0 {
		t.Errorf("別の日: %+v err=%v", other, err)
	}
}
