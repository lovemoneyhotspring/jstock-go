package broker

import (
	"fmt"
	"sort"
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

// StopCorrector は置いた逆指値の条件を後から変えられるブローカー。
// トレーリングは「逆指値を置いて、定期的にここで条件を引き上げる」形で作る。
// 発火済みの逆指値は訂正できない（立花証券のリファレンス）。
type StopCorrector interface {
	CorrectStop(clientOrderID string, brokerOrderID *string, stop domain.StopSpec) error
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

// MarginAware は信用の建玉を現物と別に照会できるブローカー。
//
// Broker の GetPositions は現物だけを返す実装があり（立花証券は
// CLMGenbutuKabuList）、信用で建てた玉が見えない。日計りの手仕舞いは
// 建玉が見えないと数量を決められないので、返せる実装だけを見分ける。
//
// 現物と信用を**別々に**取れることが要る。片方の電文が落ちても、もう片方だけを
// 使う判断（積立は現物、デイトレは信用）を続けられるようにするため。
type MarginAware interface {
	MarginPositions() ([]domain.Position, error)
}

// PositionLeg は建玉の脚。現物と信用、買建と売建を分けて数えるための鍵。
//
// 証券口座は 1 つで、積立（現物）とデイトレ（信用）が同居する。台帳は
// 戦略ごとに分かれている（state/accum-*.db と state/daytrade-*.db）が、
// 建玉は分けられないので、数えるときに脚で分ける。
//
// 銘柄コードだけを鍵にすると、積立が現物で 300 株持っている銘柄をデイトレが
// 300 株売建てた朝に合計が 0 になり、売建の持ち越しが「無い」ことになる
// （返済されないまま残る）。逆に積立の現物を「台帳外の建玉」と読んで、
// その日の発注を丸ごと止めることもある。
type PositionLeg struct {
	Symbol string
	// Margin は信用建玉か。false は現物。
	Margin bool
	// Short は売建か。現物に売り玉は無いので常に false。
	Short bool
}

// LegOf は売買区分と向きから脚を作る。trade は建てたときの区分（現物 / 信用新規）。
//
// 売買区分が空のときは現物とみなす（TradeType.IsMargin は空を信用と読むが、
// 分からないものを信用に寄せると現物の保有を信用建玉と取り違える）。
// ただし売り玉は現物にありえないので、向きが売りなら信用。
func LegOf(symbol string, trade domain.TradeType, short bool) PositionLeg {
	margin := short ||
		trade == domain.TradeTypeMarginOpen ||
		trade == domain.TradeTypeMarginClose
	return PositionLeg{Symbol: symbol, Margin: margin, Short: short}
}

// LegPosition は脚ごとに束ねた建玉。
type LegPosition struct {
	// Quantity は株数。**常に正**（売建も正）。
	Quantity decimal.Decimal
	// CostPrice は建値（同じ脚に複数の建玉があれば加重平均）。
	CostPrice decimal.Decimal
}

// LegPositions は脚ごとの建玉と、照会できなかった側の理由。
//
// 現物と信用は別の電文なので、**片方が落ちても取れた側は使える**。積立は現物、
// デイトレは信用しか見ないので、相手側の障害で判断を止めないために分けて持つ。
type LegPositions struct {
	positions map[PositionLeg]LegPosition
	// CashErr は現物を照会できなかった理由。nil なら取れている。
	CashErr error
	// MarginErr は信用建玉を照会できなかった理由。nil なら取れている。
	MarginErr error
}

// Err はその側を照会できなかった理由。margin が true なら信用、false なら現物。
func (p LegPositions) Err(margin bool) error {
	if margin {
		return p.MarginErr
	}
	return p.CashErr
}

// At は脚の建玉。その側を照会できていなければ ok = false——**0 株と「分からない」を
// 取り違えない**ため（分からないものを 0 株と読むと、建玉があっても無いことになる）。
func (p LegPositions) At(leg PositionLeg) (LegPosition, bool) {
	if p.Err(leg.Margin) != nil {
		return LegPosition{}, false
	}
	return p.positions[leg], true
}

// Legs は取れている脚を銘柄順に並べる。
func (p LegPositions) Legs() []PositionLeg {
	legs := make([]PositionLeg, 0, len(p.positions))
	for leg := range p.positions {
		legs = append(legs, leg)
	}
	sort.Slice(legs, func(i, j int) bool {
		if legs[i].Symbol != legs[j].Symbol {
			return legs[i].Symbol < legs[j].Symbol
		}
		if legs[i].Margin != legs[j].Margin {
			return !legs[i].Margin
		}
		return !legs[i].Short
	})
	return legs
}

// PositionsByLeg は現物と信用の建玉を**別々に照会して**脚ごとにまとめる。
//
// 片方が落ちても、もう片方は使える形で返す（落ちた側は At が ok = false になる）。
// 現物と信用を分けて照会できないブローカー（ペーパー口座は 1 つの一覧に混ぜている）
// では 1 回の照会で両方を賄うので、失敗すれば両方が使えない。
func PositionsByLeg(b Broker) LegPositions {
	m, split := b.(MarginAware)
	if !split {
		all, err := b.GetPositions()
		return newLegPositions(all, err, err)
	}
	cash, cashErr := b.GetPositions()
	margin, marginErr := m.MarginPositions()
	return newLegPositions(append(append([]domain.Position{}, cash...), margin...), cashErr, marginErr)
}

func newLegPositions(positions []domain.Position, cashErr, marginErr error) LegPositions {
	held := map[PositionLeg]LegPosition{}
	for _, pos := range positions {
		if pos.Quantity.IsZero() {
			continue
		}
		quantity, short := pos.Quantity, false
		if quantity.IsNegative() {
			quantity, short = quantity.Neg(), true
		}
		leg := LegOf(pos.Symbol, pos.Trade, short)
		current := held[leg]
		total := current.Quantity.Add(quantity)
		held[leg] = LegPosition{
			Quantity: total,
			CostPrice: current.CostPrice.Mul(current.Quantity).
				Add(pos.CostPrice.Mul(quantity)).Div(total),
		}
	}
	return LegPositions{positions: held, CashErr: cashErr, MarginErr: marginErr}
}
