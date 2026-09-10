package execute

import (
	"errors"
	"strings"
	"testing"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// 時価と売買単位を返すだけの控え。発注は数えるだけで送らない。
type verifyStub struct {
	broker.Broker
	lot    decimal.Decimal
	price  decimal.Decimal
	placed int
}

func (s *verifyStub) Name() string { return "stub" }
func (s *verifyStub) LotSizes([]string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{"563A": s.lot}
}
func (s *verifyStub) MarketPrices([]string) (map[string]broker.MarketPrice, error) {
	return map[string]broker.MarketPrice{"563A": {Symbol: "563A", Last: s.price}}, nil
}
func (s *verifyStub) Place(domain.OrderRequest) (*domain.OrderAck, error) {
	s.placed++
	return &domain.OrderAck{}, nil
}

func newStub(lot, price int64) *verifyStub {
	return &verifyStub{lot: decimal.NewFromInt(lot), price: decimal.NewFromInt(price)}
}

// 上限を超えたら**何も送らない**。ここが緩むと検証のつもりで大きな注文が出る。
func TestVerifyOrderRefusesOverMax(t *testing.T) {
	s := newStub(10, 277)       // 1 単元 2,770 円
	logger := &logging.Logger{} // 出力先が nil なので何も書かない
	res, err := VerifyOrder(s, nil, logger, VerifyOrderOptions{
		Symbol: "563A", MaxYen: decimal.NewFromInt(2000), Live: true,
	})
	if err == nil {
		t.Fatal("上限を超えたのにエラーにならない")
	}
	var overLimit *ErrOverLimit
	if !errors.As(err, &overLimit) {
		t.Errorf("ErrOverLimit で返っていない（異常として通知されてしまう）: %v", err)
	}
	if !strings.Contains(err.Error(), "上限") {
		t.Errorf("理由が上限だと分からない: %v", err)
	}
	if s.placed != 0 {
		t.Errorf("送ってしまっている: %d 件", s.placed)
	}
	if res == nil || !res.Estimate.Equal(decimal.NewFromInt(2770)) {
		t.Errorf("見積りが返らない: %+v", res)
	}
}

// --live が無ければ送らない。数量は 売買単位 × units。
func TestVerifyOrderDryRunBuildsRequest(t *testing.T) {
	s := newStub(1, 1006)
	res, err := VerifyOrder(s, nil, &logging.Logger{}, VerifyOrderOptions{
		Symbol: "563A", Units: 2, MaxYen: decimal.NewFromInt(3000), Live: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.placed != 0 {
		t.Errorf("dry-run で送っている: %d 件", s.placed)
	}
	if !res.Request.Quantity.Equal(decimal.NewFromInt(2)) {
		t.Errorf("数量 = %s, want 2", res.Request.Quantity)
	}
	if res.Request.Side != domain.SideBuy || res.Request.Trade != domain.TradeTypeCash {
		t.Errorf("買い・現物でない: %+v", res.Request)
	}
	if res.Request.OrderType != domain.OrderTypeLimit {
		t.Errorf("成行になっている（板の薄い銘柄で値段を掴まないよう指値のはず）: %s", res.Request.OrderType)
	}
}
