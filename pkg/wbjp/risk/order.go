package risk

import (
	"sort"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
)

// SellsFirst は売りを買いより先に並べ替えた写しを返す（売り同士・買い同士の順は保つ）。
//
// max_orders_per_day は売りにも効き、審査は渡された順に進む。Reconcile の順
// （銘柄コード順）のままだと、買いで上限を使い切ったとき損切り・手仕舞いの売りが
// 見送られる（2026-09-24 の再点検）。売りは余力を使わないので、先に出しても買いの審査は変わらない。
//
// ライブ（cmd/wbjp/run.go → execute.PlaceOrders）とバックテスト（engine）の両方がこれを
// 通す。片方だけ並べ替えると、件数の上限に当たる日に検証と実運用の判断が食い違う。
func SellsFirst(orders []domain.OrderRequest) []domain.OrderRequest {
	out := make([]domain.OrderRequest, len(orders))
	copy(out, orders)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Side == domain.SideSell && out[j].Side != domain.SideSell
	})
	return out
}
