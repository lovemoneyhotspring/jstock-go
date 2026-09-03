package fees

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(v string) decimal.Decimal { return decimal.RequireFromString(v) }

func TestCommissionTiers(t *testing.T) {
	// 段階の境目は「以下」。境目ちょうどは下の段（研究ノートと立花証券の表と同じ）
	cases := []struct{ dayTotal, want string }{
		{"0", "0"},
		{"-1", "0"},
		{"120000", "0"},
		{"120001", "176"},
		{"200000", "176"},
		{"500000", "253"},
		{"1000000", "506"},
		{"10000000", "2783"},
		// 1000 万を超えたら 100 万ごとに 253 円ずつ（端数も 1 段として数える）
		{"10000001", "3036"},
		{"11000000", "3036"},
		{"11000001", "3289"},
	}
	for _, c := range cases {
		if got := Commission(d(c.dayTotal)); !got.Equal(d(c.want)) {
			t.Errorf("Commission(%s) = %s, want %s", c.dayTotal, got, c.want)
		}
	}
}

func TestOrderFeeEstimate(t *testing.T) {
	// 67 万円の注文を往復すると合計 134 万円 → 506 + 253 = 759 円。片道はその半分
	if got := OrderFeeEstimate(d("670000")); !got.Equal(d("379.5")) {
		t.Errorf("OrderFeeEstimate = %s, want 379.5", got)
	}
	if got := OrderFeeEstimate(decimal.Zero); !got.IsZero() {
		t.Errorf("代金 0 の手数料は 0 のはず: %s", got)
	}
}

func TestRoundTripBP(t *testing.T) {
	// 5 万円の注文は往復 10 万円 → 0 円の段（12 万円まで無料）
	if got := RoundTripBP(d("50000")); !got.IsZero() {
		t.Errorf("12 万円以下の往復は 0 bp のはず: %s", got)
	}
	// 67 万円の往復は 759 円 → 759 / 670000 * 10000 ≈ 11.3 bp
	got, _ := RoundTripBP(d("670000")).Float64()
	if got < 11.2 || got > 11.4 {
		t.Errorf("RoundTripBP(670000) = %.2f bp, want ≈11.3", got)
	}
}

func TestPositionsFor(t *testing.T) {
	// 200 万 ÷ 67 万 = 2.98 → 四捨五入して 3
	n, err := PositionsFor(d("2000000"), d("670000"), 10)
	if err != nil || n != 3 {
		t.Fatalf("PositionsFor = %d, %v; want 3, nil", n, err)
	}
	// max_positions で頭打ち
	n, _ = PositionsFor(d("100000000"), d("670000"), 10)
	if n != 10 {
		t.Errorf("max_positions で頭打ちにならない: %d", n)
	}
	// 資金が 1 注文の目安の半分に届かなければエラー（設定の矛盾をここで気づかせる）
	if _, err := PositionsFor(d("100000"), d("670000"), 10); err == nil {
		t.Error("資金不足がエラーにならない")
	}
	if _, err := PositionsFor(d("0"), d("670000"), 10); err == nil {
		t.Error("資金 0 がエラーにならない")
	}
}
