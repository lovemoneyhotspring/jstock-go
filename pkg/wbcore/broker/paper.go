package broker

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// DefaultSlippageRate は成行の滑り（寄付値に対する比率）。
var DefaultSlippageRate = decimal.RequireFromString("0.001")

type holding struct {
	quantity  decimal.Decimal
	costPrice decimal.Decimal
}

// PaperBroker は約定を模擬するブローカー（検証と dry-run）。
//
// 手数料は立花証券の定額コース（FlatRateTable）: 現物はその日の約定代金の合計で段階が
// 決まるので、約定のたびに「合計が増えたぶんの差分」を取る（BeginDay で合計を戻す）。
// 信用は 0 円。発注前の見積り（Preview）とデイトレの検証（daytrade/fees）と同じ表。
type PaperBroker struct {
	mu           sync.Mutex
	initialCash  decimal.Decimal
	cash         decimal.Decimal
	slippageRate decimal.Decimal
	currency     string
	taxType      domain.TaxAccountType
	fillModel    string // "open" | "intrabar"

	holdings    map[string]*holding
	orders      map[string]domain.Order
	marks       map[string]decimal.Decimal
	fills       []domain.Fill
	realizedPnL decimal.Decimal
	boughtToday map[string]struct{}
	// cashTradedToday はその日の現物約定代金の合計（定額コースの段階の基準）。
	cashTradedToday decimal.Decimal
}

func NewPaperBroker(initialCash decimal.Decimal, fillModel string) *PaperBroker {
	if initialCash.IsZero() {
		initialCash = decimal.NewFromInt(1000000)
	}
	if fillModel == "" {
		fillModel = "open"
	}
	return &PaperBroker{
		initialCash:  initialCash,
		cash:         initialCash,
		slippageRate: DefaultSlippageRate,
		currency:     "JPY",
		taxType:      domain.TaxAccountSpecific,
		fillModel:    fillModel,
		holdings:     make(map[string]*holding),
		orders:       make(map[string]domain.Order),
		marks:        make(map[string]decimal.Decimal),
		boughtToday:  make(map[string]struct{}),
	}
}

// RealizedPnL は手数料控除後の実現損益の累計。
func (p *PaperBroker) RealizedPnL() decimal.Decimal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.realizedPnL
}

func (p *PaperBroker) Name() string {
	return "paper"
}

func (p *PaperBroker) AccountID() string {
	return "PAPER"
}

func (p *PaperBroker) cent() decimal.Decimal {
	if p.currency == "JPY" {
		return decimal.NewFromInt(1)
	}
	return decimal.RequireFromString("0.01")
}

func (p *PaperBroker) GetBalance() (*domain.Balance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	positions, _ := p.getPositionsLocked()
	unrealized := decimal.Zero
	marketVal := decimal.Zero
	for _, pos := range positions {
		unrealized = unrealized.Add(pos.UnrealizedPnL())
		marketVal = marketVal.Add(pos.MarketValue())
	}

	return &domain.Balance{
		Currency:      p.currency,
		CashBalance:   p.cash,
		BuyingPower:   p.cash,
		MarketValue:   marketVal,
		UnrealizedPnL: unrealized,
	}, nil
}

func (p *PaperBroker) getPositionsLocked() ([]domain.Position, error) {
	var positions []domain.Position
	var symbols []string
	for s := range p.holdings {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)

	for _, sym := range symbols {
		h := p.holdings[sym]
		if h.quantity.IsZero() {
			continue
		}
		lastPrice := h.costPrice
		if mark, ok := p.marks[sym]; ok {
			lastPrice = mark
		}

		trade := domain.TradeTypeCash
		if h.quantity.IsNegative() {
			trade = domain.TradeTypeMarginOpen
		}

		positions = append(positions, domain.Position{
			Symbol:            sym,
			Quantity:          h.quantity,
			AvailableQuantity: h.quantity,
			CostPrice:         h.costPrice,
			LastPrice:         lastPrice,
			Currency:          p.currency,
			TaxType:           p.taxType,
			Trade:             trade,
		})
	}
	return positions, nil
}

func (p *PaperBroker) GetPositions() ([]domain.Position, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.getPositionsLocked()
}

func (p *PaperBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	positions, err := p.GetPositions()
	if err != nil {
		return nil, err
	}
	return PositionsBySymbolHelper(positions), nil
}

func (p *PaperBroker) GetOpenOrders() ([]domain.Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var openOrders []domain.Order
	for _, o := range p.orders {
		if o.Status.IsOpen() {
			openOrders = append(openOrders, o)
		}
	}
	return openOrders, nil
}

func (p *PaperBroker) GetOrder(clientOrderID string, brokerOrderID *string) (*domain.Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if o, ok := p.orders[clientOrderID]; ok {
		return &o, nil
	}
	return nil, nil
}

func (p *PaperBroker) GetOrderHistory(start, end time.Time) ([]domain.Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var history []domain.Order
	for _, o := range p.orders {
		if o.CreatedAt == nil {
			history = append(history, o)
		} else {
			if !o.CreatedAt.Before(start) && !o.CreatedAt.After(end) {
				history = append(history, o)
			}
		}
	}
	return history, nil
}

func (p *PaperBroker) LotSizes(symbols []string) map[string]decimal.Decimal {
	return make(map[string]decimal.Decimal)
}

func (p *PaperBroker) Preview(req domain.OrderRequest) (*domain.OrderPreview, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	refPrice, err := p.referencePriceLocked(req)
	if err != nil {
		return nil, err
	}

	cost := refPrice.Mul(req.Quantity).Round(0)
	fee := p.commission(cost, req.Trade)
	return &domain.OrderPreview{
		EstimatedCost: cost,
		EstimatedFee:  fee,
	}, nil
}

func (p *PaperBroker) Place(req domain.OrderRequest) (*domain.OrderAck, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.orders[req.ClientOrderID]; ok {
		return &domain.OrderAck{
			ClientOrderID: req.ClientOrderID,
			BrokerOrderID: existing.BrokerOrderID,
			Status:        existing.Status,
		}, nil
	}

	h, exists := p.holdings[req.Symbol]
	held := decimal.Zero
	if exists {
		held = h.quantity
	}

	if req.Side == domain.SideSell && req.Trade != domain.TradeTypeMarginOpen {
		if req.Quantity.GreaterThan(held) {
			return nil, &OrderRejectedError{
				Message: fmt.Sprintf("%s: 保有 %s 株に対し %s 株の売り注文", req.Symbol, held, req.Quantity),
			}
		}
	} else if req.Side == domain.SideBuy && req.Trade == domain.TradeTypeMarginClose {
		if req.Quantity.GreaterThan(held.Neg()) {
			return nil, &OrderRejectedError{
				Message: fmt.Sprintf("%s: 売建 %s 株に対し %s 株の返済買い", req.Symbol, held.Neg(), req.Quantity),
			}
		}
	} else if req.Side == domain.SideBuy {
		refPrice, err := p.referencePriceLocked(req)
		if err != nil {
			return nil, err
		}
		cost := refPrice.Mul(req.Quantity).Round(0)
		fee := p.commission(cost, req.Trade)
		if cost.Add(fee).GreaterThan(p.cash) {
			return nil, &InsufficientFundsError{
				Message: fmt.Sprintf("%s: 必要 %s に対し買付余力 %s", req.Symbol, cost.Add(fee), p.cash),
			}
		}
	}

	now := clock.NowUTC()
	orderID := req.ClientOrderID
	order := domain.Order{
		ClientOrderID:  req.ClientOrderID,
		BrokerOrderID:  &orderID,
		Symbol:         req.Symbol,
		Side:           req.Side,
		OrderType:      req.OrderType,
		Quantity:       req.Quantity,
		FilledQuantity: decimal.Zero,
		Status:         domain.OrderStatusSubmitted,
		LimitPrice:     req.LimitPrice,
		CreatedAt:      &now,
		Trade:          req.Trade,
	}

	p.orders[order.ClientOrderID] = order
	return &domain.OrderAck{
		ClientOrderID: order.ClientOrderID,
		BrokerOrderID: order.BrokerOrderID,
		Status:        order.Status,
	}, nil
}

func (p *PaperBroker) Cancel(clientOrderID string, brokerOrderID *string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	order, ok := p.orders[clientOrderID]
	if !ok {
		return fmt.Errorf("注文が見つかりません: %s", clientOrderID)
	}
	if order.Status.IsTerminal() {
		return nil
	}
	order.Status = domain.OrderStatusCancelled
	p.orders[clientOrderID] = order
	return nil
}

func (p *PaperBroker) Mark(prices map[string]decimal.Decimal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range prices {
		p.marks[k] = v
	}
}

// AccrueInterest は待機資金に利息を付ける（短期国債・MMF に置いた想定）。
//
// annualRate は年率（0.05 = 5%）。日割りは 360 日（T-Bill の慣行）。
// 返り値は付いた利息。金利ゼロ・現金ゼロなら何もしない。
func (p *PaperBroker) AccrueInterest(annualRate decimal.Decimal, days int) decimal.Decimal {
	p.mu.Lock()
	defer p.mu.Unlock()
	if annualRate.LessThanOrEqual(decimal.Zero) || p.cash.LessThanOrEqual(decimal.Zero) || days <= 0 {
		return decimal.Zero
	}
	interest := p.cash.Mul(annualRate).Mul(decimal.NewFromInt(int64(days))).
		Div(decimal.NewFromInt(360))
	interest = roundToUnit(interest, p.cent())
	p.cash = p.cash.Add(interest)
	return interest
}

// roundToUnit は unit 刻みに丸める（円なら 1、ドルならセント）。
func roundToUnit(value, unit decimal.Decimal) decimal.Decimal {
	if unit.IsZero() {
		return value
	}
	return value.Div(unit).Round(0).Mul(unit)
}

// BeginDay は日替わり: 当日買付の記録と、定額コースの当日合計を戻す。
func (p *PaperBroker) BeginDay() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.boughtToday = make(map[string]struct{})
	p.cashTradedToday = decimal.Zero
}

// ExpireOpenOrders は未約定の注文をすべて失効させる（当日限りの注文）。
func (p *PaperBroker) ExpireOpenOrders() {
	p.ExpireOpenOrdersFor(nil)
}

// ExpireOpenOrdersFor は traded にある銘柄の未約定注文だけを失効させる。
// nil なら全銘柄。その日に立会いの無かった銘柄（休場・売買停止）の注文は
// 「出したのに約定の機会が無かった」だけなので、次の立会いまで残す。
func (p *PaperBroker) ExpireOpenOrdersFor(traded map[string]struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, o := range p.orders {
		if !o.Status.IsOpen() {
			continue
		}
		if traded != nil {
			if _, ok := traded[o.Symbol]; !ok {
				continue
			}
		}
		o.Status = domain.OrderStatusExpired
		p.orders[id] = o
	}
}

func (p *PaperBroker) Settle(openPrices map[string]decimal.Decimal, highs, lows map[string]decimal.Decimal, when *time.Time) []domain.Fill {
	p.mu.Lock()
	defer p.mu.Unlock()

	var filled []domain.Fill
	for id, order := range p.orders {
		if !order.Status.IsOpen() {
			continue
		}

		openPrice, hasOpen := openPrices[order.Symbol]
		if !hasOpen {
			continue
		}

		var high, low *decimal.Decimal
		if h, ok := highs[order.Symbol]; ok {
			high = &h
		}
		if l, ok := lows[order.Symbol]; ok {
			low = &l
		}

		execPrice := p.executionPriceLocked(order, openPrice, high, low)
		if execPrice == nil {
			continue
		}

		fill, err := p.executeLocked(order, *execPrice, when)
		if err != nil {
			order.Status = domain.OrderStatusRejected
			p.orders[id] = order
			continue
		}

		filled = append(filled, fill)
		order.Status = domain.OrderStatusFilled
		order.FilledQuantity = order.Quantity
		order.AvgFillPrice = execPrice
		p.orders[id] = order
	}

	p.fills = append(p.fills, filled...)
	return filled
}

func (p *PaperBroker) executionPriceLocked(order domain.Order, openPrice decimal.Decimal, high, low *decimal.Decimal) *decimal.Decimal {
	if order.OrderType == domain.OrderTypeMarket {
		direction := decimal.NewFromInt(1)
		if order.Side == domain.SideSell {
			direction = decimal.NewFromInt(-1)
		}
		slippage := openPrice.Mul(direction.Mul(p.slippageRate))
		price := openPrice.Add(slippage)
		return &price
	}

	limit := order.LimitPrice
	if limit == nil {
		return nil
	}

	return LimitFillPrice(order.Side, *limit, openPrice, high, low, p.fillModel)
}

func LimitFillPrice(side domain.Side, limit, openPrice decimal.Decimal, high, low *decimal.Decimal, fillModel string) *decimal.Decimal {
	if side == domain.SideBuy {
		if openPrice.LessThanOrEqual(limit) {
			return &openPrice
		}
		if fillModel == "intrabar" && low != nil && low.LessThanOrEqual(limit) {
			return &limit
		}
		return nil
	}

	// SideSell
	if openPrice.GreaterThanOrEqual(limit) {
		return &openPrice
	}
	if fillModel == "intrabar" && high != nil && high.GreaterThanOrEqual(limit) {
		return &limit
	}
	return nil
}

func (p *PaperBroker) executeLocked(order domain.Order, price decimal.Decimal, when *time.Time) (domain.Fill, error) {
	gross := price.Mul(order.Quantity).Round(0)
	fee := p.commission(gross, order.Trade)

	if order.Side == domain.SideBuy && order.Trade == domain.TradeTypeMarginClose {
		existing := p.holdings[order.Symbol]
		if existing == nil || existing.quantity.Neg().LessThan(order.Quantity) {
			return domain.Fill{}, fmt.Errorf("返済できる売建が不足")
		}
		p.realizedPnL = p.realizedPnL.Add(existing.costPrice.Sub(price).Mul(order.Quantity)).Sub(fee)
		existing.quantity = existing.quantity.Add(order.Quantity)
		p.cash = p.cash.Sub(gross).Sub(fee)
		if existing.quantity.IsZero() {
			delete(p.holdings, order.Symbol)
		}
	} else if order.Side == domain.SideBuy {
		totalNeed := gross.Add(fee)
		if totalNeed.GreaterThan(p.cash) {
			return domain.Fill{}, fmt.Errorf("必要資金不足: 必要 %s, 残高 %s", totalNeed, p.cash)
		}
		p.cash = p.cash.Sub(totalNeed)
		h, ok := p.holdings[order.Symbol]
		if !ok {
			h = &holding{quantity: decimal.Zero, costPrice: decimal.Zero}
			p.holdings[order.Symbol] = h
		}
		totalQty := h.quantity.Add(order.Quantity)
		cost := h.costPrice.Mul(h.quantity).Add(price.Mul(order.Quantity)).Div(totalQty)
		h.costPrice = cost
		h.quantity = totalQty
		p.boughtToday[order.Symbol] = struct{}{}
	} else if order.Trade == domain.TradeTypeMarginOpen {
		h, ok := p.holdings[order.Symbol]
		if !ok {
			h = &holding{quantity: decimal.Zero, costPrice: decimal.Zero}
			p.holdings[order.Symbol] = h
		}
		short := h.quantity.Neg()
		total := short.Add(order.Quantity)
		h.costPrice = h.costPrice.Mul(short).Add(price.Mul(order.Quantity)).Div(total)
		h.quantity = total.Neg()
		p.cash = p.cash.Add(gross).Sub(fee)
	} else {
		existing := p.holdings[order.Symbol]
		if existing == nil || existing.quantity.LessThan(order.Quantity) {
			return domain.Fill{}, fmt.Errorf("売却可能数量が不足")
		}
		p.realizedPnL = p.realizedPnL.Add(price.Sub(existing.costPrice).Mul(order.Quantity)).Sub(fee)
		existing.quantity = existing.quantity.Sub(order.Quantity)
		p.cash = p.cash.Add(gross).Sub(fee)
		if existing.quantity.IsZero() {
			delete(p.holdings, order.Symbol)
		}
	}

	if isCashTrade(order.Trade) {
		p.cashTradedToday = p.cashTradedToday.Add(gross)
	}
	p.marks[order.Symbol] = price
	fillTime := clock.NowUTC()
	if when != nil {
		fillTime = *when
	}

	return domain.Fill{
		ClientOrderID: order.ClientOrderID,
		Symbol:        order.Symbol,
		Side:          order.Side,
		Quantity:      order.Quantity,
		Price:         price,
		Fee:           fee,
		FilledAt:      fillTime,
	}, nil
}

func (p *PaperBroker) referencePriceLocked(req domain.OrderRequest) (decimal.Decimal, error) {
	if req.LimitPrice != nil {
		return *req.LimitPrice, nil
	}
	mark, ok := p.marks[req.Symbol]
	if !ok {
		return decimal.Zero, fmt.Errorf("%s: 成行注文の見積りに使う時価がありません。先に Mark() で設定してください", req.Symbol)
	}
	return mark, nil
}

// BoughtToday はその日に買い付けた銘柄。差金決済の回避に使う。
//
// 返すのは複製なので、呼び出し側が触っても内部状態は変わらない。
func (p *PaperBroker) BoughtToday() map[string]struct{} {
	out := make(map[string]struct{}, len(p.boughtToday))
	for sym := range p.boughtToday {
		out[sym] = struct{}{}
	}
	return out
}

// commission はこの約定で増える手数料。現物は定額コースの当日合計に対する差分、信用は 0 円。
func (p *PaperBroker) commission(gross decimal.Decimal, trade domain.TradeType) decimal.Decimal {
	if !isCashTrade(trade) {
		return decimal.Zero
	}
	return MarginalFlatRateCommission(p.cashTradedToday, gross)
}

// isCashTrade は現物か（空の取引区分は現物とみなす）。
func isCashTrade(trade domain.TradeType) bool {
	return trade == "" || trade == domain.TradeTypeCash
}
