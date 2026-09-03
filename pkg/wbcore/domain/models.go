package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// Market は市場の識別子。
type Market string

const (
	MarketJP Market = "JP"
	MarketUS Market = "US"
)

func (m Market) Currency() string {
	switch m {
	case MarketJP:
		return "JPY"
	case MarketUS:
		return "USD"
	default:
		return "JPY"
	}
}

func (m Market) Timezone() *time.Location {
	switch m {
	case MarketJP:
		loc, _ := time.LoadLocation("Asia/Tokyo")
		return loc
	case MarketUS:
		loc, _ := time.LoadLocation("America/New_York")
		return loc
	default:
		return time.UTC
	}
}

// Side は売買方向。
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType は注文種別。
type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeOther  OrderType = "OTHER"
)

func (t OrderType) IsPlaceable() bool {
	return t != OrderTypeOther
}

// TradeType は現物・信用の取引区分。
type TradeType string

const (
	TradeTypeCash        TradeType = "CASH"
	TradeTypeMarginOpen  TradeType = "MARGIN_OPEN"
	TradeTypeMarginClose TradeType = "MARGIN_CLOSE"
)

func (t TradeType) IsMargin() bool {
	return t != TradeTypeCash
}

// TaxAccountType は口座課税区分。
type TaxAccountType string

const (
	TaxAccountGeneral  TaxAccountType = "GENERAL"
	TaxAccountSpecific TaxAccountType = "SPECIFIC"
	TaxAccountNISA     TaxAccountType = "NISA"
)

// OrderStatus は注文の状態。
type OrderStatus string

const (
	OrderStatusPending         OrderStatus = "PENDING"
	OrderStatusSubmitted       OrderStatus = "SUBMITTED"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
	OrderStatusUnknown         OrderStatus = "UNKNOWN"
)

func (s OrderStatus) IsTerminal() bool {
	switch s {
	case OrderStatusFilled, OrderStatusCancelled, OrderStatusRejected, OrderStatusExpired:
		return true
	default:
		return false
	}
}

func (s OrderStatus) IsOpen() bool {
	return !s.IsTerminal()
}

// Bar は日足1本を表す。
type Bar struct {
	Symbol string
	Date   string // YYYY-MM-DD
	Open   decimal.Decimal
	High   decimal.Decimal
	Low    decimal.Decimal
	Close  decimal.Decimal
	Volume decimal.Decimal
}

func NewBar(symbol, date string, o, h, l, c, v decimal.Decimal) (Bar, error) {
	if h.LessThan(l) {
		return Bar{}, fmt.Errorf("%s %s: high < low (%s < %s)", symbol, date, h, l)
	}
	return Bar{
		Symbol: symbol,
		Date:   date,
		Open:   o,
		High:   h,
		Low:    l,
		Close:  c,
		Volume: v,
	}, nil
}

// Signal は戦略が出す意見。
type Signal struct {
	Strategy   string
	Symbol     string
	Direction  float64 // -1.0 〜 +1.0
	Confidence float64 // 0.0 〜 1.0
	Reason     string
	Meta       map[string]any
}

func NewSignal(strategy, symbol string, direction, confidence float64, reason string, meta map[string]any) (Signal, error) {
	if direction < -1.0 || direction > 1.0 {
		return Signal{}, fmt.Errorf("direction は -1.0〜1.0: %f", direction)
	}
	if confidence < 0.0 || confidence > 1.0 {
		return Signal{}, fmt.Errorf("confidence は 0.0〜1.0: %f", confidence)
	}
	if meta == nil {
		meta = make(map[string]any)
	}
	return Signal{
		Strategy:   strategy,
		Symbol:     symbol,
		Direction:  direction,
		Confidence: confidence,
		Reason:     reason,
		Meta:       meta,
	}, nil
}

func (s Signal) Score() float64 {
	return s.Direction * s.Confidence
}

// CombinedSignal は複数戦略の合成意見。
type CombinedSignal struct {
	Symbol        string
	Direction     float64 // -1.0 〜 +1.0
	Contributions map[string]float64
	Reason        string
}

func NewCombinedSignal(symbol string, direction float64, contributions map[string]float64, reason string) (CombinedSignal, error) {
	if direction < -1.0 || direction > 1.0 {
		return CombinedSignal{}, fmt.Errorf("direction は -1.0〜1.0: %f", direction)
	}
	if contributions == nil {
		contributions = make(map[string]float64)
	}
	return CombinedSignal{
		Symbol:        symbol,
		Direction:     direction,
		Contributions: contributions,
		Reason:        reason,
	}, nil
}

// TargetPosition はサイジングが決めたあるべき建玉。
type TargetPosition struct {
	Symbol   string
	Quantity decimal.Decimal
	Reason   string
}

// Position は現在の建玉。
type Position struct {
	Symbol            string
	Quantity          decimal.Decimal
	AvailableQuantity decimal.Decimal
	CostPrice         decimal.Decimal
	LastPrice         decimal.Decimal
	Currency          string
	TaxType           TaxAccountType
	Trade             TradeType
}

func (p Position) MarketValue() decimal.Decimal {
	return p.Quantity.Mul(p.LastPrice)
}

func (p Position) UnrealizedPnL() decimal.Decimal {
	return p.LastPrice.Sub(p.CostPrice).Mul(p.Quantity)
}

// Balance は口座残高。
type Balance struct {
	Currency          string
	CashBalance       decimal.Decimal
	BuyingPower       decimal.Decimal
	MarketValue       decimal.Decimal
	UnrealizedPnL     decimal.Decimal
	MarginBuyingPower *decimal.Decimal
}

func (b Balance) BuyingPowerFor(trade TradeType) decimal.Decimal {
	if trade == TradeTypeMarginOpen && b.MarginBuyingPower != nil {
		return *b.MarginBuyingPower
	}
	return b.BuyingPower
}

// MakeClientOrderID は決定論的に注文ID（32文字ハッシュ）を生成する。
// Python 版と完全一致する規則: hashlib.sha256(f"{seed_key}|{symbol}|{side.value}|{quantity}".encode()).hexdigest()[:32]
func MakeClientOrderID(seedKey, symbol string, side Side, quantity decimal.Decimal) string {
	seed := fmt.Sprintf("%s|%s|%s|%s", seedKey, symbol, string(side), quantity.String())
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:32]
}

// OrderRequest は発注リクエスト。
type OrderRequest struct {
	ClientOrderID string
	Symbol        string
	Side          Side
	OrderType     OrderType
	Quantity      decimal.Decimal
	LimitPrice    *decimal.Decimal
	TaxType       TaxAccountType
	Reason        string
	Trade         TradeType
}

func NewOrderRequest(clientOrderID, symbol string, side Side, orderType OrderType, qty decimal.Decimal, limitPrice *decimal.Decimal, taxType TaxAccountType, reason string, trade TradeType) (OrderRequest, error) {
	if qty.LessThanOrEqual(decimal.Zero) {
		return OrderRequest{}, fmt.Errorf("quantity は正の数: %s", qty)
	}
	if len(clientOrderID) > 32 {
		return OrderRequest{}, fmt.Errorf("client_order_id は32文字以内: %s", clientOrderID)
	}
	if orderType == OrderTypeLimit && limitPrice == nil {
		return OrderRequest{}, fmt.Errorf("%s には limit_price が必要", orderType)
	}
	if orderType != OrderTypeLimit && limitPrice != nil {
		return OrderRequest{}, fmt.Errorf("%s に limit_price は指定できない", orderType)
	}
	if limitPrice != nil && limitPrice.LessThanOrEqual(decimal.Zero) {
		return OrderRequest{}, fmt.Errorf("limit_price は正の数: %s", limitPrice)
	}
	if taxType == "" {
		taxType = TaxAccountGeneral
	}
	if trade == "" {
		trade = TradeTypeCash
	}
	return OrderRequest{
		ClientOrderID: clientOrderID,
		Symbol:        symbol,
		Side:          side,
		OrderType:     orderType,
		Quantity:      qty,
		LimitPrice:    limitPrice,
		TaxType:       taxType,
		Reason:        reason,
		Trade:         trade,
	}, nil
}

func (req OrderRequest) Notional() *decimal.Decimal {
	if req.LimitPrice == nil {
		return nil
	}
	n := req.Quantity.Mul(*req.LimitPrice)
	return &n
}

// OrderPreview は発注前の見積り結果。
type OrderPreview struct {
	EstimatedCost decimal.Decimal
	EstimatedFee  decimal.Decimal
}

// OrderAck は発注受付応答。
type OrderAck struct {
	ClientOrderID string
	BrokerOrderID *string
	Status        OrderStatus
}

// Order は注文の現在の状態。
type Order struct {
	ClientOrderID  string
	BrokerOrderID  *string
	Symbol         string
	Side           Side
	OrderType      OrderType
	Quantity       decimal.Decimal
	FilledQuantity decimal.Decimal
	Status         OrderStatus
	LimitPrice     *decimal.Decimal
	AvgFillPrice   *decimal.Decimal
	CreatedAt      *time.Time
	Trade          TradeType
}

func (o Order) RemainingQuantity() decimal.Decimal {
	return o.Quantity.Sub(o.FilledQuantity)
}

func (o Order) SignedRemaining() decimal.Decimal {
	sign := decimal.NewFromInt(1)
	if o.Side == SideSell {
		sign = decimal.NewFromInt(-1)
	}
	return sign.Mul(o.RemainingQuantity())
}

// Fill は約定レコード。
type Fill struct {
	ClientOrderID string
	Symbol        string
	Side          Side
	Quantity      decimal.Decimal
	Price         decimal.Decimal
	Fee           decimal.Decimal
	FilledAt      time.Time
}
