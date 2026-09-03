package main

import (
	"fmt"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newCloseCmd() *cobra.Command {
	var liveFlag, yesFlag, ignoreWindowFlag bool
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "close",
		Short: "15:20〜: 今日建てた分を成行で手仕舞う（既定は dry-run）",
		Long: "15:20〜: 今日建てた分を成行で手仕舞う——現物の買いは売却、信用の買建は返済売り、\n" +
			"売建は返済買い（15:25 以降ならクロージング・オークションで引け値）。既定は dry-run。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("引けの売り", "daytrade.crash",
				runClose(liveFlag, yesFlag, ignoreWindowFlag, dateFlag))
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "注文を出す。無ければ対象を示すだけ")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番の確認を省く（cron 用）")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "時間帯の外でも売る")
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

// exitTarget は手仕舞う 1 建玉。
type exitTarget struct {
	entry     dtledger.Order
	quantity  decimal.Decimal
	fillPrice *decimal.Decimal
}

func runClose(live, yes, ignoreWindow bool, date string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	fmt.Println(appSettings.DescribeMode(live, cfg.Execution.KillSwitch))
	now := clock.NowUTC()
	day, err := dayOrToday(date, now)
	if err != nil {
		return err
	}
	if skipHoliday(day, "close") {
		return nil
	}
	if live && !ignoreWindow && !inWindow(cfg, "exit", now) {
		fmt.Printf("手仕舞いの時間帯の外（%s）。何もしません\n", describeWindow(cfg, "exit"))
		logInfo("daytrade.skip", "手仕舞いの時間帯の外",
			map[string]any{"reason": "window", "window": describeWindow(cfg, "exit")})
		digest.Skipped("window")
		return nil
	}
	allowed, reason := appSettings.CanExecuteLive(live, cfg.Execution.KillSwitch)
	logConfig(cfg, "close", map[string]any{"day": day.Format(DateLayout), "live": live})

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

	allEntries, err := led.EntriesOn(day)
	if err != nil {
		return err
	}
	var entries []dtledger.Order
	dryRun := 0
	for _, o := range allEntries {
		if o.IsDryRun() {
			dryRun++
			continue
		}
		entries = append(entries, o)
	}
	// 生きている／約定した手仕舞いだけが「発注済み」。拒否・失効したものは数えない
	allExits, err := led.ExitsOn(day)
	if err != nil {
		return err
	}
	exits := map[string]dtledger.Order{}
	for _, o := range allExits {
		if !o.IsDryRun() && !o.IsDead() {
			exits[o.Symbol+"|"+o.Leg()] = o
		}
	}
	if len(entries) == 0 {
		fmt.Printf("今日の建玉が台帳にありません（dry-run %d 件）。何もしません\n", dryRun)
		logInfo("daytrade.skip", "売る対象なし", map[string]any{"reason": "no_buys", "dry_run": dryRun})
		if allowed {
			warnUnrecordedPositions(cfg, day)
		}
		return nil
	}

	var b broker.Broker
	if allowed {
		if b, err = connectBroker(cfg); err != nil {
			return err
		}
	}

	// 約定数量はブローカーに聞く（部分約定・拒否をそのまま手仕舞いの数量に反映する）
	//
	// 聞けなかったときに台帳の値（発注直後は 0）へ黙って落ちてはいけない。
	// 0 は「手仕舞う数量なし」として扱われるので、建玉があっても売らずに
	// 終わり、そのまま持ち越しになる。確かめられなかった銘柄は
	// unconfirmed に積んで、最後に人へ知らせたうえで異常終了する。
	var targets []exitTarget
	var unconfirmed []string
	for _, order := range entries {
		filled := order.FilledQuantity
		fillPrice := order.AvgFillPrice
		if b != nil {
			current, err := b.GetOrder(order.ClientOrderID, order.BrokerOrderID)
			if err != nil {
				fmt.Printf("  %s: 照会に失敗: %v\n", order.Symbol, err)
				current = nil
			}
			if current == nil && filled.LessThanOrEqual(decimal.Zero) &&
				domain.OrderStatus(order.Status).IsOpen() {
				// 建てたかどうかが分からない。数量を推測して売ると、
				// 建っていなかった場合に新規の反対建玉を作ってしまう。
				unconfirmed = append(unconfirmed,
					fmt.Sprintf("%s（%s / %s 株）", order.Symbol, order.Status, order.Quantity))
				logWarn("daytrade.unconfirmed", "買い注文の約定を確かめられません", map[string]any{
					"day": day.Format(DateLayout), "symbol": order.Symbol,
					"client_order_id": order.ClientOrderID, "status": order.Status,
				})
				continue
			}
			if current != nil {
				filled, fillPrice = current.FilledQuantity, current.AvgFillPrice
				fillReason := execution.ReasonExpired
				if filled.GreaterThan(decimal.Zero) {
					fillReason = execution.ReasonFilled
				}
				execution.Collect(execution.Spec{
					Event: execution.EventFill, App: "daytrade",
					Symbol: order.Symbol, Side: string(order.Side), Trade: string(order.Trade),
					ClientOrderID: order.ClientOrderID,
					BrokerOrderID: stringOf(current.BrokerOrderID),
					Live:          true, Quantity: order.Quantity,
					IntentPrice:  decimalOrNil(order.Price),
					FillQuantity: filled, FillPrice: decimalOrNil(fillPrice),
					Reason: fillReason,
				})
				_ = led.UpdateStatus(order.ClientOrderID, current.Status, filled, fillPrice, current.BrokerOrderID)
				logInfo("daytrade.fill", "買い注文の約定状況", map[string]any{
					"day": day.Format(DateLayout), "symbol": order.Symbol,
					"client_order_id": order.ClientOrderID,
					"before":          order.Status, "after": string(current.Status),
					"quantity": order.Quantity.String(), "filled": filled.String(),
				})
			} else {
				// 台帳に約定が残っている（照会は空でも過去に確定済み）。その値で手仕舞う
				logWarn("daytrade.fill", "買い注文を照会できず台帳の確定値で続行", map[string]any{
					"day": day.Format(DateLayout), "symbol": order.Symbol,
					"client_order_id": order.ClientOrderID, "filled": filled.String(),
				})
			}
		} else if filled.IsZero() &&
			(order.Status == string(domain.OrderStatusSubmitted) || order.Status == string(domain.OrderStatusPending)) {
			filled = order.Quantity // dry-run では全約定とみなして対象を示す
		}
		if filled.LessThanOrEqual(decimal.Zero) {
			fmt.Printf("  %s: 約定なし（%s）。手仕舞う数量がありません\n", order.Symbol, order.Status)
			continue
		}
		if _, done := exits[order.Symbol+"|"+order.Leg()]; done {
			fmt.Printf("  %s: 手仕舞い発注済み（冪等）\n", order.Symbol)
			continue
		}
		targets = append(targets, exitTarget{entry: order, quantity: filled, fillPrice: fillPrice})
	}
	if len(unconfirmed) > 0 {
		// 建玉が残っている可能性がある。人が板を見て手で処理する必要がある
		alert("デイトレ: 建玉の有無を確かめられません（持ち越しの恐れ）",
			strings.Join(unconfirmed, "\n"))
		digest.Anomaly("daytrade.unconfirmed_entries",
			fmt.Sprintf("%d 件の買い注文を照会できませんでした", len(unconfirmed)))
	}
	if len(targets) == 0 {
		if len(unconfirmed) > 0 {
			return fmt.Errorf("%d 件の買い注文を照会できず、手仕舞いを判断できません（口座を確認してください）",
				len(unconfirmed))
		}
		logInfo("daytrade.skip", "売る対象なし", map[string]any{"reason": "nothing_to_sell"})
		return nil
	}
	if err := confirmLive(allowed, yes); err != nil {
		return err
	}

	var failures []string
	for _, target := range targets {
		outcome, err := placeExit(cfg, led, b, target, day)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", target.entry.Symbol, err))
			fmt.Printf("  %s: 失敗 %v\n", target.entry.Symbol, err)
			outcome = "失敗"
		}
		logInfo("daytrade.order", "引けの手仕舞い注文", map[string]any{
			"day": day.Format(DateLayout), "symbol": target.entry.Symbol,
			"quantity": target.quantity.String(), "live": b != nil, "outcome": outcome,
		})
	}
	if len(failures) > 0 {
		// 売れ残りは持ち越しになる。人が手で売る必要があるので必ず知らせる
		alert(fmt.Sprintf("デイトレ: %d 件の手仕舞いが通らず（持ち越しの恐れ）", len(failures)),
			strings.Join(failures, "\n"))
		digest.Anomaly("daytrade.exit_failed", fmt.Sprintf("%d 件の手仕舞いが通らず", len(failures)))
	}
	logInfo("daytrade.run", "引けの売りを終了", map[string]any{
		"phase": "close", "live": allowed, "reason": reason,
		"sells": len(targets), "failures": len(failures),
	})
	digest.Note(map[string]any{
		"phase": "close", "live": allowed, "sells": len(targets),
		"failures": len(failures), "unconfirmed": len(unconfirmed),
	})
	if len(unconfirmed) > 0 {
		return fmt.Errorf("%d 件の買い注文を照会できませんでした（建玉が残っていないか口座を確認してください）",
			len(unconfirmed))
	}
	return nil
}

// placeExit は 1 建玉の反対売買を送る。信用で建てたものは返済、現物の買いは売却。
func placeExit(cfg dtconfig.Config, led *dtledger.Ledger, b broker.Broker, target exitTarget, day time.Time) (string, error) {
	entry := target.entry
	exitSide := domain.SideSell
	if entry.Side != domain.SideBuy {
		exitSide = domain.SideBuy
	}
	exitTrade := domain.TradeTypeCash
	if entry.Trade == domain.TradeTypeMarginOpen {
		exitTrade = domain.TradeTypeMarginClose
	}
	action := map[string]string{
		"SELL|CASH":         "売り",
		"SELL|MARGIN_CLOSE": "返済売り",
		"BUY|MARGIN_CLOSE":  "返済買い",
		"BUY|CASH":          "買い",
	}[string(exitSide)+"|"+string(exitTrade)]

	// 前回の手仕舞いが拒否されていたら種を変えて送り直す（同じ ID はブローカーが弾く）
	attempt := led.DeadCount(day, entry.Symbol, exitSide)
	seed := fmt.Sprintf("daytrade-close|%s|%d", day.Format(DateLayout), attempt)
	request := domain.OrderRequest{
		ClientOrderID: domain.MakeClientOrderID(seed, entry.Symbol, exitSide, target.quantity),
		Symbol:        entry.Symbol,
		Side:          exitSide,
		OrderType:     domain.OrderTypeMarket,
		Quantity:      target.quantity,
		TaxType:       cfg.Execution.TaxAccountType,
		Reason: fmt.Sprintf("%s %s 引けで手仕舞い（%s）",
			cfg.StrategyName(), day.Format(DateLayout), action),
		Trade: exitTrade,
	}
	if led.WasPlaced(request.ClientOrderID) {
		fmt.Printf("  %s: %s発注済み（冪等）\n", entry.Symbol, action)
		return "冪等", nil
	}
	if b == nil {
		if err := led.Record(request, day, dtledger.DryRunStatus, target.fillPrice, nil); err != nil {
			return "", err
		}
		fmt.Printf("  %s: %s %s 株 dry-run\n", entry.Symbol, action, yen(target.quantity))
		return "dry-run", nil
	}
	price := decimal.Zero
	if target.fillPrice != nil {
		price = *target.fillPrice
	}
	if err := placeRecorded(b, led, request, day, price, nil); err != nil {
		return "", err
	}
	fmt.Printf("  %s: %s %s 株 発注\n", entry.Symbol, action, yen(target.quantity))
	return "発注", nil
}

// warnUnrecordedPositions は台帳に今日の買いが無いのに、今日の候補だった銘柄を
// ブローカーが保有していれば知らせる。
//
// 台帳を失う・open が送信後に落ちて記録できない、といったときの保険。自動では売らない
// （他の戦略の保有かもしれない）。人が確かめて手で売る。
func warnUnrecordedPositions(cfg dtconfig.Config, day time.Time) {
	p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
	if err != nil || !ok {
		return
	}
	symbols := map[string]struct{}{}
	for _, c := range p.Candidates {
		if c.Eligible || c.ShortEligible {
			symbols[c.Symbol] = struct{}{}
		}
	}
	b, err := connectBroker(cfg)
	if err != nil {
		return
	}
	// 信用建玉も見る。立花の GetPositions は現物しか返さないので、
	// これを使わないと日計りで建てた玉が「無い」ことになる。
	positions, err := broker.PositionsBySymbolIncludingMargin(b)
	if err != nil {
		logWarn("daytrade.reconcile", "建玉を照会できず保険の確認を省略", map[string]any{"error": err.Error()})
		return
	}
	held := map[string]decimal.Decimal{}
	for symbol, position := range positions {
		if _, ok := symbols[symbol]; ok && !position.Quantity.IsZero() {
			held[symbol] = position.Quantity
		}
	}
	if len(held) == 0 {
		return
	}
	var parts []string
	names := make([]string, 0, len(held))
	for symbol := range held {
		names = append(names, symbol)
	}
	sortStrings(names)
	detail := map[string]any{}
	for _, symbol := range names {
		quantity := held[symbol]
		suffix := ""
		if quantity.IsNegative() {
			suffix = "（売建）"
		}
		parts = append(parts, fmt.Sprintf("%s %s 株%s", symbol, yen(quantity), suffix))
		detail[symbol] = quantity.String()
	}
	text := strings.Join(parts, "、")
	fmt.Printf("台帳に無い建玉があります（今日の候補の銘柄）: %s。手で確かめてください\n", text)
	logError("daytrade.reconcile", "台帳に無い建玉",
		map[string]any{"day": day.Format(DateLayout), "held": detail})
	alert("デイトレ: 台帳に無い建玉（持ち越しの恐れ）", text)
}

func stringOf(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func decimalOrNil(v *decimal.Decimal) any {
	if v == nil {
		return nil
	}
	return *v
}
