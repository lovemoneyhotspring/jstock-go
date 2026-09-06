package broker

import (
	"fmt"
	"sort"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/shopspring/decimal"
)

// 立花証券の残高・建玉・見積り。項目名は削除済み Python 実装
// （git show ac1eb7a:src/wbcore/broker/tachibana.py）に合わせてある。

// 現物保有（CLMGenbutuKabuList）の項目。売却の電文と共通の接頭辞 sUriOrder が付く。
const (
	fieldCashIssue     = "sUriOrderIssueCode"
	fieldCashQty       = "sUriOrderZanKabuSuryou"
	fieldCashAvailable = "sUriOrderUritukeKanouSuryou"
	fieldCashCost      = "sUriOrderGaisanBokaTanka"
	fieldCashLast      = "sUriOrderHyoukaTanka"
	fieldCashTax       = "sUriOrderZyoutoekiKazeiC"
)

// 信用建玉（CLMShinyouTategyokuList）の項目。
const (
	fieldMarginIssue     = "sOrderIssueCode"
	fieldMarginQty       = "sOrderTategyokuSuryou"
	fieldMarginAvailable = "sOrderHensaiKanouSuryou"
	fieldMarginCost      = "sOrderTategyokuTanka"
	fieldMarginLast      = "sOrderHyoukaTanka"
	fieldMarginTax       = "sOrderZyoutoekiKazeiC"
	fieldMarginNumber    = "sOrderTategyokuNumber"
	fieldMarginDay       = "sOrderTategyokuDay"
)

// 可能額サマリー（CLMZanKaiSummary）の項目。
const (
	fieldCashBuyingPower   = "sGenbutuKabuKaituke"  // 現物買付可能額
	fieldCashTradedToday   = "sGenbutuBaibaiDaikin" // 当日の現物約定代金（手数料の段階に効く）
	fieldMarginBuyingPower = "sSinyouSinkidate"     // 信用新規建可能額
)

// jst は東京。
func jst() *time.Location { return clock.Tokyo }

// today は JST の今日。
func (t *TachibanaBroker) today() time.Time {
	return clock.ToZone(clock.NowUTC(), clock.Tokyo)
}

// GetBalance は可能額サマリー。
func (t *TachibanaBroker) GetBalance() (*domain.Balance, error) {
	res, err := t.postRequest(clmBalanceSummary, map[string]any{})
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmBalanceSummary); err != nil {
		return nil, err
	}
	if _, ok := res[fieldCashBuyingPower]; !ok {
		return nil, &ErrUnverifiedResponse{
			CLMID: clmBalanceSummary, Expected: fieldCashBuyingPower, Got: keysOf(res)}
	}

	cash := fieldDecimal(res, fieldCashBuyingPower)
	// 定額コースの手数料は当日の現物約定代金の合計で決まる。見積りのために覚えておく
	t.cashTradedToday = fieldDecimal(res, fieldCashTradedToday)

	balance := &domain.Balance{
		Currency:    "JPY",
		CashBalance: cash,
		BuyingPower: cash,
	}
	if margin, ok := fieldDecimalOK(res, fieldMarginBuyingPower); ok {
		balance.MarginBuyingPower = &margin
	}
	return balance, nil
}

// GetPositions は現物の保有。信用建玉は MarginPositions。
// 両方を脚ごとに束ねたいときは PositionsByLeg（片方が落ちても他方を使える）。
func (t *TachibanaBroker) GetPositions() ([]domain.Position, error) {
	res, err := t.postRequest(clmCashPositions, map[string]any{"sIssueCode": ""})
	if err != nil {
		return nil, err
	}
	if err := checkResult(res, clmCashPositions); err != nil {
		return nil, err
	}
	rows, err := rowsOf(res, cashPositionsKey, clmCashPositions)
	if err != nil {
		return nil, err
	}

	positions := make([]domain.Position, 0, len(rows))
	for _, row := range rows {
		quantity := fieldDecimal(row, fieldCashQty)
		if quantity.IsZero() {
			continue
		}
		available := quantity
		if v, ok := fieldDecimalOK(row, fieldCashAvailable); ok {
			available = v
		}
		positions = append(positions, domain.Position{
			Symbol:            field(row, fieldCashIssue),
			Quantity:          quantity,
			AvailableQuantity: available,
			CostPrice:         fieldDecimal(row, fieldCashCost),
			LastPrice:         fieldDecimal(row, fieldCashLast),
			Currency:          "JPY",
			TaxType:           ParseTaxAccount(field(row, fieldCashTax), domain.TaxAccountSpecific),
			Trade:             domain.TradeTypeCash,
		})
	}
	return positions, nil
}

func (t *TachibanaBroker) PositionsBySymbol() (map[string]domain.Position, error) {
	positions, err := t.GetPositions()
	if err != nil {
		return nil, err
	}
	return PositionsBySymbolHelper(positions), nil
}

// marginRows は信用建玉の生の行。symbol が空なら全銘柄。
func (t *TachibanaBroker) marginRows(symbol string) ([]map[string]any, error) {
	res, err := t.postRequest(clmMarginPositions, map[string]any{"sIssueCode": symbol})
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
	if symbol == "" {
		return rows, nil
	}
	filtered := rows[:0:0]
	for _, row := range rows {
		if field(row, fieldMarginIssue) == symbol {
			filtered = append(filtered, row)
		}
	}
	return filtered, nil
}

// MarginPositions は信用の建玉。**売建は数量を負で返す**（PaperBroker と同じ約束）。
func (t *TachibanaBroker) MarginPositions() ([]domain.Position, error) {
	rows, err := t.marginRows("")
	if err != nil {
		return nil, err
	}

	positions := make([]domain.Position, 0, len(rows))
	for _, row := range rows {
		quantity := fieldDecimal(row, fieldMarginQty)
		if quantity.IsZero() {
			continue
		}
		side, err := ParseSide(field(row, fieldSideKubun))
		if err != nil {
			return nil, fmt.Errorf("建玉の売買区分を解釈できません: %w", err)
		}
		sign := decimal.NewFromInt(1)
		if side == domain.SideSell {
			sign = decimal.NewFromInt(-1)
		}
		available := quantity
		if v, ok := fieldDecimalOK(row, fieldMarginAvailable); ok {
			available = v
		}
		positions = append(positions, domain.Position{
			Symbol:            field(row, fieldMarginIssue),
			Quantity:          sign.Mul(quantity),
			AvailableQuantity: sign.Mul(available),
			CostPrice:         fieldDecimal(row, fieldMarginCost),
			LastPrice:         fieldDecimal(row, fieldMarginLast),
			Currency:          "JPY",
			TaxType:           ParseTaxAccount(field(row, fieldMarginTax), domain.TaxAccountSpecific),
			Trade:             domain.TradeTypeMarginOpen,
			BrokerPositionID:  field(row, fieldMarginNumber),
		})
	}
	return positions, nil
}

// Preview は 1 注文の見積り。見積り電文は無いので手元で出す。
//
// 手数料は信用 0 円。現物は**定額コース**（1 日の現物合計 12 万円まで 0 円）の
// 前提で、当日の既約定分にこの注文を足したときの増分を見積もる。
// 個別手数料コースの口座では過小になる（コースは Web で選ぶ）。
func (t *TachibanaBroker) Preview(req domain.OrderRequest) (*domain.OrderPreview, error) {
	var cost decimal.Decimal
	if req.LimitPrice != nil {
		cost = req.LimitPrice.Mul(req.Quantity).Round(0)
	} else {
		// 成行は時価で見積もる。0 円として返すと買付余力の突き合わせが素通りする
		prices, err := t.MarketPrices([]string{req.Symbol})
		if err != nil {
			return nil, fmt.Errorf("成行の見積りに時価を取れません (%s): %w", req.Symbol, err)
		}
		quote, ok := prices[req.Symbol]
		if !ok || !quote.Last.IsPositive() {
			return nil, fmt.Errorf("成行の見積りに使える時価がありません: %s", req.Symbol)
		}
		cost = quote.Last.Mul(req.Quantity).Round(0)
	}

	if req.Trade.IsMargin() || cost.LessThanOrEqual(decimal.Zero) {
		return &domain.OrderPreview{EstimatedCost: cost, EstimatedFee: decimal.Zero}, nil
	}
	// 当日の現物約定代金を最新にする（GetBalance が cashTradedToday を更新する）
	if _, err := t.GetBalance(); err != nil {
		return nil, err
	}
	fee := MarginalFlatRateCommission(t.cashTradedToday, cost)
	return &domain.OrderPreview{EstimatedCost: cost, EstimatedFee: fee}, nil
}

// repaymentList は返済する建玉を個別指定する。
//
// 反対側の建玉を**建日の新しい順（当日優先）**に割り当てる。日計りは当日建てた
// 玉を返すので、古い玉から返すと持ち越しが増える。
// 足りなければ発注せずエラー——現物売りとして通すと、持っていない株を売る。
func (t *TachibanaBroker) repaymentList(req domain.OrderRequest) ([]map[string]string, error) {
	// 買戻し（返済買い）なら売建、返済売りなら買建を返す
	wanted := domain.SideSell
	if req.Side == domain.SideSell {
		wanted = domain.SideBuy
	}

	rows, err := t.marginRows(req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("返済する建玉を照会できません (%s): %w", req.Symbol, err)
	}
	return allocateRepayment(rows, req, wanted, t.today().Format("20060102"))
}

// allocateRepayment は建玉の行から返済の割り当てを作る。
//
// 通信を伴わないので、割り当ての規則だけを単体で確かめられる。
func allocateRepayment(
	rows []map[string]any,
	req domain.OrderRequest,
	wanted domain.Side,
	todayJST string,
) ([]map[string]string, error) {
	var candidates []map[string]any
	for _, row := range rows {
		side, err := ParseSide(field(row, fieldSideKubun))
		if err != nil {
			return nil, fmt.Errorf("建玉の売買区分を解釈できません: %w", err)
		}
		if side == wanted {
			candidates = append(candidates, row)
		}
	}

	// 当日の建玉を先に、その中では建日の新しい順
	sort.SliceStable(candidates, func(i, j int) bool {
		di, dj := field(candidates[i], fieldMarginDay), field(candidates[j], fieldMarginDay)
		if (di == todayJST) != (dj == todayJST) {
			return di == todayJST
		}
		return di > dj
	})

	remaining := req.Quantity
	var allocation []map[string]string
	for _, row := range candidates {
		if remaining.LessThanOrEqual(decimal.Zero) {
			break
		}
		available := fieldDecimal(row, fieldMarginAvailable)
		if available.LessThanOrEqual(decimal.Zero) {
			continue
		}
		number := field(row, fieldMarginNumber)
		if number == "" {
			return nil, &ErrUnverifiedResponse{
				CLMID: clmMarginPositions, Expected: fieldMarginNumber, Got: keysOf(row)}
		}
		use := decimal.Min(available, remaining)
		allocation = append(allocation, map[string]string{
			"sTategyokuNumber": number,
			"sTatebiZyuni":     fmt.Sprint(len(allocation) + 1),
			"sOrderSuryou":     use.String(),
		})
		remaining = remaining.Sub(use)
	}

	if remaining.IsPositive() {
		what := "買建"
		if wanted == domain.SideSell {
			what = "売建"
		}
		return nil, &OrderRejectedError{Message: fmt.Sprintf(
			"%s: 返済できる%sが %s 株しかありません（要求 %s 株）",
			req.Symbol, what, req.Quantity.Sub(remaining), req.Quantity)}
	}
	return allocation, nil
}
