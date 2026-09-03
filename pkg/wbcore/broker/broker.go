package broker

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

type Broker interface {
	Name() string
	AccountID() string
	GetBalance() (*domain.Balance, error)
	GetPositions() ([]domain.Position, error)
	GetOpenOrders() ([]domain.Order, error)
	GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error)
	GetOrderHistory(start, end time.Time) ([]domain.Order, error)
	Preview(req domain.OrderRequest) (*domain.OrderPreview, error)
	Place(req domain.OrderRequest) (*domain.OrderAck, error)
	Cancel(clientOrderID string, brokerOrderID *string) error
	LotSizes(symbols []string) map[string]decimal.Decimal
	PositionsBySymbol() (map[string]domain.Position, error)
}

// PositionsBySymbolHelper は建玉一覧を銘柄コードごとのマップ（同一銘柄の加重平均取得単価と数量合算）にまとめる。
func PositionsBySymbolHelper(positions []domain.Position) map[string]domain.Position {
	merged := make(map[string]domain.Position)
	for _, pos := range positions {
		existing, ok := merged[pos.Symbol]
		if !ok {
			merged[pos.Symbol] = pos
			continue
		}

		totalQty := existing.Quantity.Add(pos.Quantity)
		cost := existing.CostPrice
		if !totalQty.IsZero() {
			existingTotal := existing.CostPrice.Mul(existing.Quantity)
			newTotal := pos.CostPrice.Mul(pos.Quantity)
			cost = existingTotal.Add(newTotal).Div(totalQty)
		}

		merged[pos.Symbol] = domain.Position{
			Symbol:            pos.Symbol,
			Quantity:          totalQty,
			AvailableQuantity: existing.AvailableQuantity.Add(pos.AvailableQuantity),
			CostPrice:         cost,
			LastPrice:         pos.LastPrice,
			Currency:          pos.Currency,
			TaxType:           existing.TaxType,
			Trade:             existing.Trade,
		}
	}
	return merged
}

// BrokerError はブローカー処理の基底エラー。
type BrokerError struct {
	Message string
}

func (e *BrokerError) Error() string {
	return e.Message
}

type OrderRejectedError struct {
	Message string
}

func (e *OrderRejectedError) Error() string {
	return fmt.Sprintf("order rejected: %s", e.Message)
}

type InsufficientFundsError struct {
	Message string
}

func (e *InsufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: %s", e.Message)
}
