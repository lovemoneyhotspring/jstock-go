package main

import (
	"fmt"
	"strings"

	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newVerifyCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "引け後: 手仕舞いが全部約定したかを照会し、未約定（持ち越し）なら通知する",
		Long: "引け後: 今日の手仕舞いが全部約定したかをブローカーに照会し、未約定（持ち越し）なら\n" +
			"通知する。ロング（買い→売り）もショート（売建→返済買い）も脚ごとに突き合わせる。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("手仕舞いの検証", "daytrade.crash", runVerify(dateFlag))
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

func runVerify(date string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	day, err := dayOrToday(date, clock.NowUTC())
	if err != nil {
		return err
	}
	logConfig(cfg, "verify", map[string]any{"day": day.Format(DateLayout)})

	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	defer func() {
		if err := execution.Flush(historyStore(), day); err != nil {
			logWarn("daytrade.execution", "実行品質の記録に失敗", map[string]any{"error": err.Error()})
		}
	}()

	entries, err := livePart(led.EntriesOn(day))
	if err != nil {
		return err
	}
	exits, err := livePart(led.ExitsOn(day))
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("今日の本発注はありません")
		logInfo("daytrade.skip", "検証の対象なし", map[string]any{"reason": "no_buys", "phase": "verify"})
		return nil
	}

	b, err := connectBroker(cfg)
	if err != nil {
		return err
	}

	// 照会できなかった注文は数えておく。verify の役目は持ち越しの検出なので、
	// 「確かめられなかった」を「持ち越し無し」として通すと役目を果たさない。
	var unconfirmed []string
	noteUnconfirmed := func(order dtledger.Order, err error) {
		reason := "応答に該当の注文がありません"
		if err != nil {
			reason = err.Error()
		}
		unconfirmed = append(unconfirmed, fmt.Sprintf("%s %s: %s", order.Symbol, order.Leg(), reason))
		logWarn("daytrade.unconfirmed", "注文を照会できません", map[string]any{
			"day": day.Format(DateLayout), "symbol": order.Symbol,
			"client_order_id": order.ClientOrderID, "error": reason,
		})
	}

	opened := map[string]decimal.Decimal{}
	for _, order := range entries {
		key := order.Symbol + "|" + order.Leg()
		filled := order.FilledQuantity
		current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
		if err != nil || current == nil {
			if domain.OrderStatus(order.Status).IsOpen() {
				noteUnconfirmed(order, err)
			}
		} else {
			filled = current.FilledQuantity
			_ = led.UpdateStatus(order.ClientOrderID, current.Status, filled, current.AvgFillPrice, current.BrokerOrderID)
		}
		opened[key] = opened[key].Add(filled)
	}
	closed := map[string]decimal.Decimal{}
	for _, order := range exits {
		key := order.Symbol + "|" + order.Leg()
		filled := order.FilledQuantity
		current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
		if err != nil || current == nil {
			if domain.OrderStatus(order.Status).IsOpen() {
				noteUnconfirmed(order, err)
			}
		}
		if current != nil {
			filled = current.FilledQuantity
			_ = led.UpdateStatus(order.ClientOrderID, current.Status, filled, current.AvgFillPrice, current.BrokerOrderID)
			logInfo("daytrade.fill", "手仕舞い注文の約定状況", map[string]any{
				"day": day.Format(DateLayout), "symbol": order.Symbol,
				"side": string(order.Side), "trade": string(order.Trade),
				"client_order_id": order.ClientOrderID,
				"before":          order.Status, "after": string(current.Status),
				"quantity": order.Quantity.String(), "filled": filled.String(),
			})
		}
		closed[key] = closed[key].Add(filled)
	}

	var carried []string
	flagged := map[string]struct{}{}
	keys := make([]string, 0, len(opened))
	for key := range opened {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		symbol, leg, _ := strings.Cut(key, "|")
		what := "買い"
		if leg == "short" {
			what = "売建"
		}
		remaining := opened[key].Sub(closed[key])
		switch {
		case remaining.GreaterThan(decimal.Zero):
			carried = append(carried, fmt.Sprintf("%s %s %s 株", symbol, what, yen(remaining)))
			flagged[symbol] = struct{}{}
			fmt.Printf("  %s: %s %s 株が手仕舞えていません（持ち越し）\n", symbol, what, yen(remaining))
		case opened[key].GreaterThan(decimal.Zero):
			fmt.Printf("  %s: %s %s 株 手仕舞い済み\n", symbol, what, yen(opened[key]))
		}
	}

	// 台帳と食い違う建玉（送信結果不明の注文が実は通っていた等）はブローカー側で確かめる
	positions, err := broker.PositionsBySymbolIncludingMargin(b)
	if err != nil {
		logWarn("daytrade.reconcile", "建玉を照会できず突合を省略", map[string]any{"error": err.Error()})
		positions = nil
		unconfirmed = append(unconfirmed, fmt.Sprintf("建玉の照会に失敗: %v", err))
	}
	for _, key := range keys {
		symbol, leg, _ := strings.Cut(key, "|")
		if _, already := flagged[symbol]; already {
			continue
		}
		held, ok := positions[symbol]
		if !ok {
			continue
		}
		// ロングの脚なら正の建玉、ショートの脚なら負（売建）の建玉が残っていれば不一致
		leftover := held.Quantity.GreaterThan(decimal.Zero)
		if leg == "short" {
			leftover = held.Quantity.LessThan(decimal.Zero)
		}
		if !leftover {
			continue
		}
		carried = append(carried, fmt.Sprintf("%s %s 株（台帳と不一致）", symbol, yen(held.Quantity)))
		fmt.Printf("  %s: ブローカーに %s 株の建玉（台帳では手仕舞い済み）\n", symbol, yen(held.Quantity))
		logError("daytrade.reconcile", "台帳と建玉が不一致", map[string]any{
			"day": day.Format(DateLayout), "symbol": symbol, "leg": leg,
			"held": held.Quantity.String(),
		})
	}

	if len(carried) > 0 {
		logError("daytrade.carry", "持ち越し",
			map[string]any{"day": day.Format(DateLayout), "positions": carried})
		digest.Anomaly("daytrade.carry", fmt.Sprintf("%d 銘柄が未返済のまま", len(carried)))
		alert("デイトレ: 売れ残りがあります（持ち越し）。翌朝に手で売ってください",
			strings.Join(carried, "\n"))
		return nil
	}

	// 照会できなかった注文があるなら「持ち越しなし」とは言えない。
	// ここで正常終了すると、確かめられなかっただけの日と本当に無事な日が
	// ログの上で区別できなくなる。
	if len(unconfirmed) > 0 {
		logError("daytrade.unconfirmed", "照会できず持ち越しを判定できません",
			map[string]any{"day": day.Format(DateLayout), "orders": unconfirmed})
		digest.Anomaly("daytrade.unconfirmed",
			fmt.Sprintf("%d 件を照会できず持ち越しを判定できません", len(unconfirmed)))
		alert("デイトレ: 注文を照会できず持ち越しを判定できません。口座を確認してください",
			strings.Join(unconfirmed, "\n"))
		return fmt.Errorf("%d 件の注文を照会できませんでした", len(unconfirmed))
	}

	fmt.Println("手仕舞いを確認しました（持ち越しなし）")
	logInfo("daytrade.run", "手仕舞いを確認",
		map[string]any{"phase": "verify", "live": true, "carried": 0})
	return nil
}

// livePart は dry-run を除いた注文。
func livePart(orders []dtledger.Order, err error) ([]dtledger.Order, error) {
	if err != nil {
		return nil, err
	}
	out := make([]dtledger.Order, 0, len(orders))
	for _, o := range orders {
		if !o.IsDryRun() {
			out = append(out, o)
		}
	}
	return out, nil
}
