// 発注経路の実機検証（docs/BROKER_VERIFY.md）のための、1 単元だけの注文。
//
// なぜ要るか: 未検証で残っているのは注文照会（CLMOrderList）の項目名——約定数量・
// 約定単価・注文状態——で、これは**実際に 1 件約定させないと確かめられない**。
// 一方 `accum run` が注文を作るのは月初の入金日と増額日だけなので、検証したい日に
// 注文が出ない。BROKER_VERIFY.md の手順にはこの穴があった。
//
// 安全のための約束:
//   - 買いだけ。売り・信用・逆指値は出さない
//   - 数量は「売買単位 × units」。units の既定は 1
//   - **見積り金額が MaxYen を超えたら送らない**（呼び出し側の既定 2,000 円）
//   - 成行は使わず指値（現在値）。板が薄い銘柄で想定外の値段を掴まない
//   - 台帳には verify 印を付けて記録する（成績の集計から外れる）

package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// priceSource は時価問合を持つブローカー（立花）。Broker インターフェースには
// 時価が無いので、ここで型アサーションして使う。
type priceSource interface {
	MarketPrices(symbols []string) (map[string]broker.MarketPrice, error)
}

// VerifyOrderOptions は検証用の 1 単元注文の指定。
type VerifyOrderOptions struct {
	// Symbol は銘柄コード（"563A" のような 4〜5 桁。".T" は付けない）。
	Symbol string
	// Units は売買単位の何倍か（既定 1）。
	Units int
	// MaxYen は見積り金額の上限。これを超えたら送らない。
	MaxYen decimal.Decimal
	// Live が偽なら送らずに「何を送るか」だけ出す。
	Live bool
}

// VerifyOrderResult は送った注文と、その後の照会の結果。
type VerifyOrderResult struct {
	Request  domain.OrderRequest
	Estimate decimal.Decimal
	Lot      decimal.Decimal
	Price    decimal.Decimal
	// Ack は発注の応答（Live が偽なら nil）。
	Ack *domain.OrderAck
	// Queried は発注直後に照会し直した注文（照会できなければ nil）。
	Queried *domain.Order
}

// VerifyOrder は 1 単元だけ買い、直後に照会して項目名が読めるかを確かめる。
func VerifyOrder(
	b broker.Broker,
	led *ledger.Ledger,
	logger *logging.Logger,
	opts VerifyOrderOptions,
) (*VerifyOrderResult, error) {
	if opts.Symbol == "" {
		return nil, fmt.Errorf("銘柄を指定してください")
	}
	units := opts.Units
	if units < 1 {
		units = 1
	}
	if opts.MaxYen.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("上限金額（--max-yen）は正の数で指定してください")
	}

	// 売買単位と現在値。どちらも取れなければ数量も金額も決められないので止める
	lots := b.LotSizes([]string{opts.Symbol})
	lot, ok := lots[opts.Symbol]
	if !ok || !lot.IsPositive() {
		return nil, fmt.Errorf("%s の売買単位を取得できません（CLMStkGetIssueMstKabu）", opts.Symbol)
	}
	prices, ok := b.(priceSource)
	if !ok {
		return nil, fmt.Errorf("このブローカー（%s）は時価問合を持たないので検証注文は作れません", b.Name())
	}
	quotes, err := prices.MarketPrices([]string{opts.Symbol})
	if err != nil {
		return nil, fmt.Errorf("%s の時価を取得できません: %w", opts.Symbol, err)
	}
	quote, found := quotes[opts.Symbol]
	if !found || !quote.Last.IsPositive() {
		return nil, fmt.Errorf("%s の現在値が取れません", opts.Symbol)
	}

	qty := lot.Mul(decimal.NewFromInt(int64(units)))
	estimate := quote.Last.Mul(qty)
	result := &VerifyOrderResult{Estimate: estimate, Lot: lot, Price: quote.Last}

	// **上限の柵。** ここを越えたら何も送らない
	if estimate.GreaterThan(opts.MaxYen) {
		return result, fmt.Errorf(
			"見積り %s 円が上限 %s 円を超えます（%s: 売買単位 %s × %d 単元 × %s 円）。"+
				"銘柄を安いものに変えるか --max-yen を上げてください",
			estimate.Round(0), opts.MaxYen.Round(0), opts.Symbol, lot, units, quote.Last)
	}

	price := quote.Last
	orderID := domain.MakeClientOrderID(
		clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02"),
		opts.Symbol, domain.SideBuy, qty)
	req, err := domain.NewOrderRequest(
		orderID, opts.Symbol, domain.SideBuy, domain.OrderTypeLimit, qty, &price,
		domain.TaxAccountSpecific, "発注経路の実機検証（1 単元）", domain.TradeTypeCash)
	if err != nil {
		return result, err
	}
	result.Request = req

	if !opts.Live {
		logger.Info("accum.verify_dry_run", fmt.Sprintf(
			"[dry-run] %s を %s 株 @ %s 円（見積り %s 円 / 上限 %s 円）",
			opts.Symbol, qty, price, estimate.Round(0), opts.MaxYen.Round(0)))
		return result, nil
	}

	// 買付余力の確認。足りないまま送っても拒否されるだけなので、先に見て止める
	preview, err := b.Preview(req)
	if err != nil {
		return result, fmt.Errorf("見積りに失敗しました: %w", err)
	}
	if bal, berr := b.GetBalance(); berr == nil && bal != nil {
		need := preview.EstimatedCost.Add(preview.EstimatedFee)
		if need.GreaterThan(bal.BuyingPower) {
			return result, fmt.Errorf("買付余力が足りません（必要 %s 円 / 余力 %s 円）。入金してから実行してください",
				need.Round(0), bal.BuyingPower.Round(0))
		}
	}

	month := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01") + "-01"
	market := domain.MarketJP
	ack, err := placeRecorded(b, led, req, month, estimate, market)
	if err != nil {
		return result, err
	}
	result.Ack = ack
	logger.Info("accum.verify_order", fmt.Sprintf("検証の発注に成功: %s %s 株（ID: %s）",
		opts.Symbol, qty, ack.ClientOrderID))

	// ここが本題——**送った注文を照会し直して項目名が読めるか**を見る。
	// 少し待つのは、送った直後は一覧に出ないことがあるため
	time.Sleep(2 * time.Second)
	order, qerr := b.GetOrder(req.ClientOrderID, ack.BrokerOrderID)
	if qerr != nil {
		logger.Warn("accum.verify_query_failed", fmt.Sprintf("発注は成功したが照会できません: %v", qerr))
		return result, nil
	}
	result.Queried = order
	return result, nil
}
