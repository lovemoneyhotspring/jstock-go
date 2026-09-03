package broker

import (
	"fmt"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 立花証券の売買区分・現金信用区分と、信用の建玉。
//
// **実機未検証。** 区分コードは資料と既存実装（Place の "3"=買 / "1"=売）に
// 合わせてある。返済（MARGIN_CLOSE）は建玉の指定が要るため、指定できない
// ときは発注せずエラーにする——現物売りとして通すと、持っていない株を
// 売ろうとすることになる。

const (
	tachibanaSideBuy  = "3"
	tachibanaSideSell = "1"

	// 現金信用区分。
	kubunCash        = "0" // 現物
	kubunMarginOpen  = "2" // 信用新規（制度）
	kubunMarginClose = "4" // 信用返済（制度）

	// 譲渡益課税区分。
	zeiSpecific = "1" // 特定口座
	zeiGeneral  = "2" // 一般口座
	zeiNISA     = "3" // NISA
)

// 建玉一覧の応答。実機で確認したらここだけ直す。
const marginPositionsKey = "aCLMShinyouTategyoku"

// sideKubunOf は売買区分。
func sideKubunOf(side domain.Side) string {
	if side == domain.SideSell {
		return tachibanaSideSell
	}
	return tachibanaSideBuy
}

// tradeKubunOf は現金信用区分。未知の取引種別は現物に落とさずエラーにする。
func tradeKubunOf(trade domain.TradeType) (string, error) {
	switch trade {
	case domain.TradeTypeCash, "":
		return kubunCash, nil
	case domain.TradeTypeMarginOpen:
		return kubunMarginOpen, nil
	case domain.TradeTypeMarginClose:
		return kubunMarginClose, nil
	default:
		return "", fmt.Errorf("立花証券に送れない取引種別です: %s", trade)
	}
}

// tradeTypeFromKubun は現金信用区分を domain の取引種別に戻す（注文照会用）。
func tradeTypeFromKubun(kubun string, side domain.Side) domain.TradeType {
	switch strings.TrimSpace(kubun) {
	case kubunMarginOpen:
		return domain.TradeTypeMarginOpen
	case kubunMarginClose:
		return domain.TradeTypeMarginClose
	default:
		return domain.TradeTypeCash
	}
}

// zeiKubunOf は譲渡益課税区分。
func zeiKubunOf(tax domain.TaxAccountType) string {
	switch tax {
	case domain.TaxAccountGeneral:
		return zeiGeneral
	case domain.TaxAccountNISA:
		return zeiNISA
	default:
		return zeiSpecific
	}
}

// jst は東京。
func jst() *time.Location { return clock.Tokyo }

// today は JST の今日。
func (t *TachibanaBroker) today() time.Time {
	return clock.ToZone(clock.NowUTC(), clock.Tokyo)
}

// MarginPositions は信用の建玉を返す。
//
// GetPositions（現物）とは別の電文。日計りの返済はこの建玉を指定して出す。
func (t *TachibanaBroker) MarginPositions() ([]domain.Position, error) {
	res, err := t.postRequest(clmMarginPositions, nil)
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmMarginPositions); err != nil {
		return nil, err
	}
	rows, err := rowsOf(res, marginPositionsKey, clmMarginPositions)
	if err != nil {
		return nil, err
	}

	positions := make([]domain.Position, 0, len(rows))
	for _, row := range rows {
		symbol := field(row, fieldIssueCode)
		if symbol == "" {
			continue
		}
		quantity := fieldDecimal(row, "sTategyokuSuryo", "sZanSuryo", "sOrderSuryo")
		if quantity.IsZero() {
			continue
		}
		// 売建は数量を負で持つ（PaperBroker と同じ約束）
		if field(row, fieldSideKubun) == tachibanaSideSell {
			quantity = quantity.Neg()
		}
		positions = append(positions, domain.Position{
			Symbol:            symbol,
			Quantity:          quantity,
			AvailableQuantity: fieldDecimal(row, "sTategyokuHensaiKanouSuryo", "sTategyokuSuryo"),
			CostPrice:         fieldDecimal(row, "sTategyokuTanka", "sYakuzyouTanka"),
			LastPrice:         fieldDecimal(row, "sGenzaiNe"),
			Currency:          "JPY",
			TaxType:           domain.TaxAccountSpecific,
			Trade:             domain.TradeTypeMarginOpen,
			// 返済の指定に要る建玉番号。Place（返済）が使う。
			BrokerPositionID: fieldAny(row, "sTategyokuNumber", "sTategyokuNo"),
		})
	}
	return positions, nil
}

// AllPositions は現物と信用建玉をまとめて返す。
//
// daytrade（信用）は建玉が見えないと手仕舞いの数量を決められない。
// GetPositions が現物しか返さないので、両方要る場面はこちらを使う。
func (t *TachibanaBroker) AllPositions() ([]domain.Position, error) {
	cash, err := t.GetPositions()
	if err != nil {
		return nil, err
	}
	margin, err := t.MarginPositions()
	if err != nil {
		return nil, err
	}
	return append(cash, margin...), nil
}

// Preview は 1 注文の見積り。
//
// 成行（指値なし）は時価で見積もる。0 円として返すと、買付余力との
// 突き合わせが常に通ってしまう。手数料は定額コースの段階表から出す。
func (t *TachibanaBroker) Preview(req domain.OrderRequest) (*domain.OrderPreview, error) {
	price := decimal.Zero
	if req.LimitPrice != nil {
		price = *req.LimitPrice
	} else {
		prices, err := t.MarketPrices([]string{req.Symbol})
		if err != nil {
			return nil, fmt.Errorf("成行の見積りに時価を取れません (%s): %w", req.Symbol, err)
		}
		quote, ok := prices[req.Symbol]
		if !ok || !quote.Last.IsPositive() {
			return nil, fmt.Errorf("成行の見積りに使える時価がありません: %s", req.Symbol)
		}
		price = quote.Last
	}

	cost := price.Mul(req.Quantity).Round(0)

	// 信用取引は手数料 0 円（日計り）。現物は定額コースの段階表。
	fee := decimal.Zero
	if req.Trade == domain.TradeTypeCash || req.Trade == "" {
		// 1 注文だけを往復した日として見積もる（段階の境目を跨ぐかで変わる）
		fee = FlatRateCommission(cost.Mul(decimal.NewFromInt(2))).Div(decimal.NewFromInt(2)).Round(0)
	}

	return &domain.OrderPreview{EstimatedCost: cost, EstimatedFee: fee}, nil
}
