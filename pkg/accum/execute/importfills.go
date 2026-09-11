// 手で発注した約定を台帳に取り込む。
//
// なぜ要るか: 積立の台帳は「当月いくら投下したか」の記録で、`accum plan` と
// `accum run` はこれを基準に「あといくら買うか」を決める。ブローカーの画面から
// 手で買った約定は台帳に入らないので、放っておくと
//
//   - 二重買付ガード（UnrecordedFills）が毎回止める（正しいが、積立が動かない）
//   - 止めずに通せば、当月の予算をもう一度買う
//
// のどちらかになる。手で買ったぶんを台帳に写して、記録を実態に合わせる。
//
// 取り込みの単位は**注文番号**。client_order_id にそれを入れるので、
// 2 度叩いても同じ行を上書きするだけで重複しない。

package execute

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
)

// ImportFillsOptions は取り込みの指定。
type ImportFillsOptions struct {
	// Symbols は取り込む銘柄。空なら履歴にある買いをすべて見る。
	Symbols []string
	// Orders は取り込む注文番号（"番号/営業日" または番号だけ）。空なら絞らない。
	//
	// 同じ銘柄に検証の注文と本当の買いが混じる日があるので、番号で選べるようにしてある
	// （検証のぶんを投下額として入れると当月の記録がずれる）。
	Orders []string
	// Apply が偽なら台帳を書かず、何を入れるかだけ返す。
	Apply bool
}

// ImportedFill は取り込む（取り込んだ）約定 1 件。
type ImportedFill struct {
	Symbol        string
	BrokerOrderID string
	Quantity      decimal.Decimal
	AvgFillPrice  decimal.Decimal
	// Amount は約定額（株数 × 約定単価）。台帳の「発注済み」に数える額。
	Amount decimal.Decimal
}

// ImportFills は当月の買い約定のうち台帳に無いものを台帳に写す。
//
// 見るのは**買い・約定あり**だけ。売りは積立の投下額ではないので入れない。
func ImportFills(
	led *ledger.Ledger,
	b broker.Broker,
	logger *logging.Logger,
	opts ImportFillsOptions,
) ([]ImportedFill, error) {
	known, err := led.RecordedIDs()
	if err != nil {
		return nil, fmt.Errorf("台帳の注文 ID を読めません: %w", err)
	}
	knownBroker, err := led.BrokerOrderIDs()
	if err != nil {
		return nil, fmt.Errorf("台帳の注文番号を読めません: %w", err)
	}

	wanted := make(map[string]struct{}, len(opts.Symbols))
	for _, s := range opts.Symbols {
		wanted[s] = struct{}{}
	}
	// 注文番号は「番号/営業日」でも番号だけでも指定できるようにする
	wantedOrders := make(map[string]struct{}, len(opts.Orders))
	for _, o := range opts.Orders {
		wantedOrders[o] = struct{}{}
		if number, _, ok := splitOrderNumber(o); ok {
			wantedOrders[number] = struct{}{}
		}
	}

	jst := clock.ToZone(clock.NowUTC(), clock.Tokyo)
	monthStart := time.Date(jst.Year(), jst.Month(), 1, 0, 0, 0, 0, clock.Tokyo)
	history, err := b.GetOrderHistory(monthStart, jst)
	if err != nil {
		return nil, fmt.Errorf("ブローカーの注文履歴を照会できません: %w", err)
	}

	planMonth := monthStart.Format("2006-01-02")
	market := domain.MarketJP

	var found []ImportedFill
	for _, o := range history {
		if o.Side != domain.SideBuy || !o.FilledQuantity.IsPositive() {
			continue
		}
		if len(wanted) > 0 {
			if _, ok := wanted[o.Symbol]; !ok {
				continue
			}
		}
		brokerID := o.ClientOrderID
		if o.BrokerOrderID != nil && *o.BrokerOrderID != "" {
			brokerID = *o.BrokerOrderID
		}
		if len(wantedOrders) > 0 && !matchesOrder(wantedOrders, brokerID) {
			continue
		}
		// 既に台帳にあるものは触らない（自分が出した注文はこちらで除かれる）
		if _, ok := known[o.ClientOrderID]; ok {
			continue
		}
		if _, ok := knownBroker[brokerID]; ok {
			continue
		}
		if _, ok := known[brokerID]; ok {
			continue
		}
		// 約定単価が無い行は金額が決められない。黙って 0 円で入れると
		// 当月の投下額が過小になるので、取り込まずに知らせる
		if o.AvgFillPrice == nil || !o.AvgFillPrice.IsPositive() {
			logger.Warn("accum.import_no_price", fmt.Sprintf(
				"%s（注文番号 %s）は約定単価が取れないので取り込みません", o.Symbol, brokerID))
			continue
		}
		fill := ImportedFill{
			Symbol:        o.Symbol,
			BrokerOrderID: brokerID,
			Quantity:      o.FilledQuantity,
			AvgFillPrice:  *o.AvgFillPrice,
			Amount:        o.FilledQuantity.Mul(*o.AvgFillPrice),
		}
		found = append(found, fill)
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].Symbol != found[j].Symbol {
			return found[i].Symbol < found[j].Symbol
		}
		return found[i].BrokerOrderID < found[j].BrokerOrderID
	})

	if !opts.Apply {
		return found, nil
	}

	for _, fill := range found {
		// client_order_id に注文番号を入れる。取り込みを 2 度やっても同じ行になる
		req := domain.OrderRequest{
			ClientOrderID: fill.BrokerOrderID,
			Symbol:        fill.Symbol,
			Side:          domain.SideBuy,
			OrderType:     domain.OrderTypeOther,
			Quantity:      fill.Quantity,
			Trade:         domain.TradeTypeCash,
			Reason:        fmt.Sprintf("手で発注したぶんの取り込み（注文番号 %s）", fill.BrokerOrderID),
		}
		brokerID := fill.BrokerOrderID
		amount := fill.Amount
		if err := led.Record(req, string(domain.OrderStatusFilled), &brokerID, &planMonth, &amount, &market); err != nil {
			return found, fmt.Errorf("%s（注文番号 %s）を台帳に書けません: %w", fill.Symbol, brokerID, err)
		}
		qty := fill.Quantity
		price := fill.AvgFillPrice
		if err := led.UpdateStatusDetail(req.ClientOrderID, string(domain.OrderStatusFilled),
			&qty, &price, &brokerID, &amount); err != nil {
			return found, fmt.Errorf("%s（注文番号 %s）の約定を台帳に書けません: %w", fill.Symbol, brokerID, err)
		}
		logger.Info("accum.import_fill", fmt.Sprintf("取り込み: %s %s 株 @ %s 円 = %s 円（注文番号 %s）",
			fill.Symbol, qty, price, amount.Round(0), brokerID))
	}
	return found, nil
}

// splitOrderNumber は "番号/営業日" を分ける（営業日が無ければ ok は偽）。
func splitOrderNumber(value string) (number, day string, ok bool) {
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 {
		return value, "", false
	}
	return parts[0], parts[1], true
}

// matchesOrder は注文番号が指定に合うか。"番号/営業日" と番号だけのどちらでも拾う。
func matchesOrder(wanted map[string]struct{}, brokerID string) bool {
	if _, ok := wanted[brokerID]; ok {
		return true
	}
	number, _, _ := splitOrderNumber(brokerID)
	_, ok := wanted[number]
	return ok
}
