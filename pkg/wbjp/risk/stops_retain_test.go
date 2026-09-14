package risk

import (
	"reflect"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// TestRetainHeldDropsClosedPositions は、手仕舞った（建玉 0・一覧に無い）銘柄のストップを外し、
// 同じ銘柄を建て直したとき新しい建値・作成日でストップが作られること。
func TestRetainHeldDropsClosedPositions(t *testing.T) {
	old := decimal.NewFromInt(900)
	sb := NewStopBook(map[string]*Stop{
		"7203": {Symbol: "7203", StopPrice: decimal.NewFromInt(1900), EntryPrice: decimal.NewFromInt(2000), CreatedOn: "2026-08-01"},
		"6758": {Symbol: "6758", StopPrice: old, EntryPrice: decimal.NewFromInt(1000), CreatedOn: "2026-06-01"},
		"9984": {Symbol: "9984", StopPrice: old, EntryPrice: decimal.NewFromInt(1000), CreatedOn: "2026-06-01"},
	})
	positions := map[string]domain.Position{
		"7203": {Symbol: "7203", Quantity: decimal.NewFromInt(100), CostPrice: decimal.NewFromInt(2000)},
		"9984": {Symbol: "9984", Quantity: decimal.Zero},
	}
	removed := sb.RetainHeld(positions)
	if !reflect.DeepEqual(removed, []string{"6758", "9984"}) {
		t.Errorf("外した銘柄: %v", removed)
	}
	if _, ok := sb.Get("7203"); !ok || sb.Len() != 1 {
		t.Errorf("保有中のストップまで外した: len=%d", sb.Len())
	}

	// 6758 を建て直すと、古いストップを引き継がず今日の建値で作られる
	positions["6758"] = domain.Position{Symbol: "6758", Quantity: decimal.NewFromInt(100),
		CostPrice: decimal.NewFromInt(3000), LastPrice: decimal.NewFromInt(3000)}
	sb.EnsureWithOptions(positions, map[string]decimal.Decimal{"6758": decimal.NewFromInt(50)}, "2026-09-14",
		EnsureOptions{ATRMultiple: decimal.NewFromInt(2)})
	st, ok := sb.Get("6758")
	if !ok || st.CreatedOn != "2026-09-14" || !st.EntryPrice.Equal(decimal.NewFromInt(3000)) || !st.StopPrice.Equal(decimal.NewFromInt(2900)) {
		t.Errorf("建て直しで古いストップを引き継いだ: %+v ok=%v", st, ok)
	}

	if removed := sb.RetainHeld(nil); len(removed) != 2 || sb.Len() != 0 {
		t.Errorf("建玉なしなら全部外す: %v", removed)
	}
}
