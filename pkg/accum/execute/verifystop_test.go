package execute

import (
	"errors"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// stopStub は verifyStub に現物建玉を足した控え。
type stopStub struct {
	*verifyStub
	held decimal.Decimal
}

func (s *stopStub) PositionsBySymbol() (map[string]domain.Position, error) {
	if !s.held.IsPositive() {
		return map[string]domain.Position{}, nil
	}
	return map[string]domain.Position{"563A": {Symbol: "563A", Quantity: s.held}}, nil
}

func newStopStub(lot, price, held int64) *stopStub {
	return &stopStub{verifyStub: newStub(lot, price), held: decimal.NewFromInt(held)}
}

// 持っていない株には逆指値を置かない（置けば新規の空売りになる）。
// かつ柵なので ErrNoPosition で返す（異常として通知しないため）。
func TestVerifyStopRefusesWithoutPosition(t *testing.T) {
	s := newStopStub(1, 1000, 0)
	_, err := VerifyStop(s, &logging.Logger{}, VerifyStopOptions{Symbol: "563A", Live: true})
	if err == nil {
		t.Fatal("保有が無いのにエラーにならない")
	}
	var noPos *ErrNoPosition
	if !errors.As(err, &noPos) {
		t.Errorf("ErrNoPosition で返っていない（異常として通知されてしまう）: %v", err)
	}
	if s.placed != 0 {
		t.Errorf("送ってしまっている: %d 件", s.placed)
	}
}

// 保有が数量に足りない場合も送らない。
func TestVerifyStopRefusesPartialPosition(t *testing.T) {
	s := newStopStub(100, 1000, 50) // 1 単元 100 株に対し 50 株しか無い
	if _, err := VerifyStop(s, &logging.Logger{}, VerifyStopOptions{Symbol: "563A", Live: true}); err == nil {
		t.Fatal("保有不足なのにエラーにならない")
	}
	if s.placed != 0 {
		t.Errorf("送ってしまっている: %d 件", s.placed)
	}
}

// dry-run は送らず、売り・現物・逆指値だけ（発火後は成行）の注文を組む。
// 条件価格は現在値より下で、呼値に乗っていること。
func TestVerifyStopDryRunBuildsRequest(t *testing.T) {
	s := newStopStub(1, 1000, 1)
	res, err := VerifyStop(s, &logging.Logger{}, VerifyStopOptions{
		Symbol: "563A", DropPct: decimal.NewFromInt(3), Live: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.placed != 0 {
		t.Errorf("dry-run で送っている: %d 件", s.placed)
	}
	req := res.Request
	if req.Side != domain.SideSell || req.Trade != domain.TradeTypeCash {
		t.Errorf("売り・現物でない: %+v", req)
	}
	if req.Stop == nil {
		t.Fatal("逆指値が付いていない")
	}
	if !req.IsStopOnly() {
		t.Error("「逆指値だけ」になっていない（発火前に板に出てしまう）")
	}
	if req.Stop.Price != nil {
		t.Errorf("発火後が成行でない: %s", req.Stop.Price)
	}
	// 1,000 円の −3% = 970 円。呼値 1 円なのでそのまま
	if !res.Trigger.Equal(decimal.NewFromInt(970)) {
		t.Errorf("条件価格 = %s, want 970", res.Trigger)
	}
	if res.Trigger.GreaterThanOrEqual(s.price) {
		t.Errorf("条件価格が現在値以上（置いた瞬間に発火する）: %s", res.Trigger)
	}
}
