// 逆指値の実機検証（docs/BROKER_VERIFY.md の手順 5）。
//
// なぜ要るか: 逆指値は「発注（CLMKabuNewOrder の逆指値）→ 照会（sOrderGyakusasi* /
// sOrderTriggerType）→ 訂正（CLMKabuCorrectOrder）→ 取消」の 4 つが未検証で、
// トレーリング（pkg/wbjp/risk/stops.go）はこの経路に乗っている。項目名が違えば
// 「ストップを置いたつもりで置けていない」が黙って通る。
//
// 安全のための約束:
//   - 保有している現物を売る逆指値だけ。新規の買いは出さない（お金を使わない）
//   - 数量は保有数量を超えない。既定は売買単位 1 単元
//   - 条件価格は現在値から --drop-pct ぶん**下**（既定 3%）。発火しない水準に置く
//   - 最後に必ず取消す。取消せなかったら警告して終わる（玉を抱えたままにしない）
//   - 台帳は触らない。積立の台帳は買い（投下額）の記録で、売りを入れると有効額が狂う

package execute

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	"github.com/shopspring/decimal"
)

// ErrNoPosition は売る玉が無いので逆指値を置かなかった、というエラー。
//
// これは**柵が正しく働いた結果**であって異常ではない（ErrOverLimit と同じ扱い）。
// Crash に渡すと Discord に alert が飛び、夜間の自己修復と日次レポートが本当の
// 異常として拾う。呼び出し側はこの型だけ通常のエラー終了にする。
type ErrNoPosition struct {
	Symbol string
	Want   decimal.Decimal
	Have   decimal.Decimal
}

func (e *ErrNoPosition) Error() string {
	return fmt.Sprintf("%s の保有が足りません（必要 %s 株 / 保有 %s 株）。"+
		"先に `accum verify-order --symbol %s --live -y` で 1 単元買って、約定を待ってください",
		e.Symbol, e.Want, e.Have, e.Symbol)
}

// VerifyStopOptions は逆指値の検証の指定。
type VerifyStopOptions struct {
	// Symbol は銘柄コード（保有していること）。
	Symbol string
	// Units は売買単位の何倍を売る逆指値にするか（既定 1）。
	Units int
	// DropPct は条件価格を現在値から何 % 下に置くか（既定 3）。
	DropPct decimal.Decimal
	// Live が偽なら送らずに「何を送るか」だけ出す。
	Live bool
}

// VerifyStopStep は 1 段（発注・照会・訂正・取消）の結果。
type VerifyStopStep struct {
	Name string
	// OK が偽なら Detail に失敗の理由が入る。以降の段は実行しない（取消は必ず試す）。
	OK     bool
	Detail string
}

// VerifyStopResult は各段の結果と、置いた注文。
type VerifyStopResult struct {
	Request domain.OrderRequest
	Trigger decimal.Decimal
	// Corrected は訂正後の条件価格。
	Corrected decimal.Decimal
	Steps     []VerifyStopStep
}

func (r *VerifyStopResult) add(name string, ok bool, format string, args ...any) {
	r.Steps = append(r.Steps, VerifyStopStep{Name: name, OK: ok, Detail: fmt.Sprintf(format, args...)})
}

// VerifyStop は保有している現物に売りの逆指値を置き、照会・訂正・取消まで通す。
func VerifyStop(
	b broker.Broker,
	logger *logging.Logger,
	opts VerifyStopOptions,
) (*VerifyStopResult, error) {
	if opts.Symbol == "" {
		return nil, fmt.Errorf("銘柄を指定してください")
	}
	units := opts.Units
	if units < 1 {
		units = 1
	}
	dropPct := opts.DropPct
	if !dropPct.IsPositive() {
		dropPct = decimal.NewFromInt(3)
	}
	result := &VerifyStopResult{}

	// 1. 売買単位と現在値
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

	// 2. **保有の確認。** 持っていない株の売りは新規の空売りになるので、必ず止める
	held, perr := b.PositionsBySymbol()
	if perr != nil {
		return nil, fmt.Errorf("現物建玉を照会できません（持っていない株を売らないため中止します）: %w", perr)
	}
	pos, hasPos := held[opts.Symbol]
	if !hasPos || pos.Quantity.LessThan(qty) {
		have := decimal.Zero
		if hasPos {
			have = pos.Quantity
		}
		return result, &ErrNoPosition{Symbol: opts.Symbol, Want: qty, Have: have}
	}

	// 3. 条件価格。現在値から dropPct ぶん下げて、呼値に乗せる
	raw := quote.Last.Mul(decimal.NewFromInt(100).Sub(dropPct)).Div(decimal.NewFromInt(100))
	trigger, err := marketrules.SnapToTick(raw, domain.SideSell, false, marketrules.RoundingConservative)
	if err != nil {
		return nil, fmt.Errorf("条件価格を呼値に乗せられません: %w", err)
	}
	result.Trigger = trigger

	orderID := domain.MakeClientOrderID(
		clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02"),
		opts.Symbol, domain.SideSell, qty)
	req, err := domain.NewOrderRequest(
		orderID, opts.Symbol, domain.SideSell, domain.OrderTypeMarket, qty, nil,
		domain.TaxAccountSpecific, "逆指値経路の実機検証（1 単元・発火しない水準）", domain.TradeTypeCash)
	if err != nil {
		return result, err
	}
	// 逆指値だけ（発火するまで板に出ない）。発火後は成行
	req, err = req.WithStop(trigger, nil)
	if err != nil {
		return result, err
	}
	result.Request = req

	if !opts.Live {
		logger.Info("accum.verify_stop_dry_run", fmt.Sprintf(
			"[dry-run] %s を %s 株 売り逆指値 条件 %s 円（現在値 %s 円 の −%s%%）",
			opts.Symbol, qty, trigger, quote.Last, dropPct))
		result.add("発注", true, "dry-run（送っていません）")
		return result, nil
	}

	// 4. 発注
	ack, err := b.Place(req)
	if err != nil {
		result.add("発注", false, "%v", err)
		return result, fmt.Errorf("逆指値の発注が通りませんでした: %w", err)
	}
	result.add("発注", true, "受理（注文番号 %s）", brokerOrderIDText(ack.BrokerOrderID))
	logger.Info("accum.verify_stop", fmt.Sprintf("逆指値を置きました: %s %s 株 条件 %s 円（ID: %s）",
		opts.Symbol, qty, trigger, ack.ClientOrderID))

	// ここから先は何があっても取消を試す
	defer func() {
		if cerr := b.Cancel(req.ClientOrderID, ack.BrokerOrderID); cerr != nil {
			result.add("取消", false, "%v", cerr)
			logger.Warn("accum.verify_stop_cancel_failed", fmt.Sprintf(
				"逆指値を取消せませんでした。手で取消してください（ID: %s / 注文番号 %s）: %v",
				req.ClientOrderID, brokerOrderIDText(ack.BrokerOrderID), cerr))
			return
		}
		time.Sleep(2 * time.Second)
		if order, qerr := b.GetOrder(req.ClientOrderID, ack.BrokerOrderID); qerr != nil {
			result.add("取消", true, "取消は通ったが照会できません: %v", qerr)
		} else {
			result.add("取消", true, "状態 %s", order.Status)
		}
	}()

	// 5. 照会——逆指値の項目が読めるか
	time.Sleep(2 * time.Second)
	order, qerr := b.GetOrder(req.ClientOrderID, ack.BrokerOrderID)
	switch {
	case qerr != nil:
		result.add("照会", false, "%v", qerr)
		return result, nil
	case order.Stop == nil:
		result.add("照会", false, "状態 %s だが Stop が nil（逆指値の項目名が読めていない）", order.Status)
		return result, nil
	default:
		result.add("照会", true, "状態 %s  条件 %s 円  発火 %v",
			order.Status, order.Stop.Trigger, order.StopTriggered)
	}

	// 6. 訂正——条件価格をさらに 2% 下げ、一覧に反映されるか
	lower := trigger.Mul(decimal.NewFromInt(98)).Div(decimal.NewFromInt(100))
	lower, err = marketrules.SnapToTick(lower, domain.SideSell, false, marketrules.RoundingConservative)
	if err != nil {
		result.add("訂正", false, "条件価格を呼値に乗せられません: %v", err)
		return result, nil
	}
	result.Corrected = lower
	corrector, ok := b.(broker.StopCorrector)
	if !ok {
		result.add("訂正", false, "このブローカー（%s）は CorrectStop を持ちません", b.Name())
		return result, nil
	}
	if err := corrector.CorrectStop(req.ClientOrderID, ack.BrokerOrderID, domain.StopSpec{Trigger: lower}); err != nil {
		result.add("訂正", false, "%v", err)
		return result, nil
	}
	time.Sleep(2 * time.Second)
	order, qerr = b.GetOrder(req.ClientOrderID, ack.BrokerOrderID)
	switch {
	case qerr != nil:
		result.add("訂正", false, "訂正は通ったが照会できません: %v", qerr)
	case order.Stop == nil:
		result.add("訂正", false, "訂正後の照会で Stop が nil")
	case !order.Stop.Trigger.Equal(lower):
		result.add("訂正", false, "条件が %s 円のまま（%s 円に変えたはず）", order.Stop.Trigger, lower)
	default:
		result.add("訂正", true, "条件 %s 円 → %s 円 が照会に反映された", trigger, lower)
	}
	return result, nil
}

// brokerOrderIDText は注文番号を表示用にする（無ければ「—」）。
func brokerOrderIDText(id *string) string {
	if id == nil || *id == "" {
		return "—"
	}
	return *id
}
