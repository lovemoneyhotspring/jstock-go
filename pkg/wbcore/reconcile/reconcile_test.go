package reconcile

import (
	"testing"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

var (
	t0  = time.Date(2026, 9, 4, 0, 1, 0, 0, time.UTC)
	now = t0.Add(time.Minute)
)

func order(id, symbol string, side domain.Side, qty int64, trade domain.TradeType, created *time.Time) domain.Order {
	return domain.Order{BrokerOrderID: &id, Symbol: symbol, Side: side, Quantity: decimal.NewFromInt(qty),
		Trade: trade, Status: domain.OrderStatusFilled, FilledQuantity: decimal.NewFromInt(qty), CreatedAt: created}
}

func pending(id, symbol string, side domain.Side, qty int64, placed time.Time) Pending {
	return Pending{ClientOrderID: id, Symbol: symbol, Side: side, Trade: domain.TradeTypeCash,
		Quantity: decimal.NewFromInt(qty), PlacedAt: placed}
}

func TestResolveOutcomes(t *testing.T) {
	created := t0.Add(2 * time.Second)
	todays := []domain.Order{
		order("1/20260904", "7203", domain.SideBuy, 100, domain.TradeTypeCash, &created),
		order("2/20260904", "9984", domain.SideBuy, 200, domain.TradeTypeCash, &created), // 数量が違う
		order("3/20260904", "6758", domain.SideBuy, 100, domain.TradeTypeCash, &created), // 台帳が既に知っている
	}
	pendings := []Pending{
		pending("a", "7203", domain.SideBuy, 100, t0),
		pending("b", "9984", domain.SideBuy, 100, t0),
		pending("c", "6758", domain.SideBuy, 100, t0),
		pending("d", "8306", domain.SideBuy, 100, t0),
		pending("e", "4063", domain.SideBuy, 100, now),
	}
	got := Resolve(pendings, todays, Options{Now: now, Grace: DefaultGrace,
		Known: map[string]struct{}{"3/20260904": {}}})
	want := map[string]Outcome{"a": Attributed, "b": Ambiguous, "c": NotSent, "d": NotSent, "e": TooRecent}
	for _, r := range got {
		if want[r.Pending.ClientOrderID] != r.Outcome {
			t.Errorf("%s: %s, want %s（%s）", r.Pending.ClientOrderID, r.Outcome, want[r.Pending.ClientOrderID], r.Reason)
		}
		if r.Outcome == Attributed && (r.Match == nil || *r.Match.BrokerOrderID != "1/20260904") {
			t.Errorf("帰属先: %+v", r.Match)
		}
	}
	s := Summarize(got)
	if s.Attributed != 1 || s.NotSent != 2 || s.Ambiguous != 1 || s.TooRecent != 1 {
		t.Errorf("集計: %+v", s)
	}
}

func TestResolveAssignsEachBrokerOrderOnce(t *testing.T) {
	c1, c2 := t0.Add(time.Second), t0.Add(3*time.Minute)
	todays := []domain.Order{
		order("1/x", "7203", domain.SideBuy, 100, domain.TradeTypeCash, &c1),
		order("2/x", "7203", domain.SideBuy, 100, domain.TradeTypeCash, &c2),
	}
	// 同じ注文を 2 回送った（1 回目は結果不明、2 回目は種を変えた）→ 時刻順に 1 つずつ
	pendings := []Pending{
		pending("late", "7203", domain.SideBuy, 100, t0.Add(3*time.Minute)),
		pending("early", "7203", domain.SideBuy, 100, t0),
	}
	got := Resolve(pendings, todays, Options{Now: now.Add(time.Hour)})
	if len(got) != 2 || got[0].Pending.ClientOrderID != "early" || *got[0].Match.BrokerOrderID != "1/x" ||
		got[1].Outcome != Attributed || *got[1].Match.BrokerOrderID != "2/x" {
		t.Errorf("%+v", got)
	}
	// ブローカーに 1 件しか無ければ、後の方は NotSent
	got = Resolve(pendings, todays[:1], Options{Now: now.Add(time.Hour)})
	if got[0].Outcome != Attributed || got[1].Outcome != NotSent {
		t.Errorf("%+v", got)
	}
}

func TestResolveIgnoresOrdersFromLongBefore(t *testing.T) {
	old := t0.Add(-time.Hour)
	todays := []domain.Order{order("1/x", "7203", domain.SideBuy, 100, domain.TradeTypeCash, &old)}
	got := Resolve([]Pending{pending("a", "7203", domain.SideBuy, 100, t0)}, todays, Options{Now: now})
	if got[0].Outcome != NotSent {
		t.Errorf("1 時間前からある注文に帰属した: %+v", got[0])
	}
}
