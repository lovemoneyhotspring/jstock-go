package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/execution"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/history"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newOpenCmd() *cobra.Command {
	var (
		liveFlag         bool
		yesFlag          bool
		ignoreWindowFlag bool
		allowDelayedFlag bool
		quoteSourceFlag  string
		quoteFileFlag    string
		dateFlag         string
	)
	cmd := &cobra.Command{
		Use:   "open",
		Short: "9:00: 気配でギャップ下位 N 銘柄を選び、成行で買う（既定は dry-run）",
		Long: "9:00: 気配でギャップ下位 N 銘柄を選び、成行で買う。\n" +
			"[margin] が有効ならギャップ上位の貸借銘柄を信用で売建てる。既定は dry-run。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return crash("寄付の買い", "daytrade.crash", runOpen(openOptions{
				live: liveFlag, yes: yesFlag, ignoreWindow: ignoreWindowFlag,
				allowDelayed: allowDelayedFlag, quoteSource: quoteSourceFlag,
				quoteFile: quoteFileFlag, date: dateFlag,
			}))
		},
	}
	cmd.Flags().BoolVar(&liveFlag, "live", false, "注文を出す。無ければ判断だけ行い、注文は出さない")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番の確認を省く（cron 用）")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "時間帯の外でも判断する")
	cmd.Flags().BoolVar(&allowDelayedFlag, "allow-delayed", false, "遅延した気配でも使う（検証用）")
	cmd.Flags().StringVar(&quoteSourceFlag, "quote-source", "", "気配の取得元を上書き（tachibana / csv）")
	cmd.Flags().StringVar(&quoteFileFlag, "quote-file", "", "csv のときのファイル")
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}

type openOptions struct {
	live, yes, ignoreWindow, allowDelayed bool
	quoteSource, quoteFile, date          string
}

func runOpen(opts openOptions) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if !cfg.Capital.Enabled {
		fmt.Println("jp_gap_fade は無効（capital.enabled = false）。何もしません")
		logInfo("daytrade.skip", "戦略が無効", map[string]any{"reason": "disabled"})
		digest.Skipped("disabled")
		return nil
	}
	fmt.Println(appSettings.DescribeMode(opts.live, cfg.Execution.KillSwitch))

	now := clock.NowUTC()
	day, err := dayOrToday(opts.date, now)
	if err != nil {
		return err
	}
	watchOnly := cfg.Capital.Positions() == 0
	logConfig(cfg, "open", map[string]any{
		"day": day.Format(DateLayout), "live": opts.live,
		"allow_delayed": opts.allowDelayed, "quote_override": opts.quoteSource,
		"watch_only": watchOnly,
	})
	if watchOnly {
		fmt.Println("資金 0（max_capital = 0）: スクリーニングと候補の表示だけ行い、買いません")
	}
	if skipHoliday(day, "open") {
		return nil
	}
	if opts.live && !opts.ignoreWindow && !inWindow(cfg, "entry", now) {
		fmt.Printf("発注時間帯の外（%s）。何もしません\n", describeWindow(cfg, "entry"))
		logInfo("daytrade.skip", "発注時間帯の外",
			map[string]any{"reason": "window", "window": describeWindow(cfg, "entry")})
		digest.Skipped("window")
		return nil
	}

	p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("%s の候補が無いので今作ります（前夜の plan が走っていません）\n", day.Format(DateLayout))
		if p, err = buildPlan(cfg, day); err != nil {
			return err
		}
	}
	p = refreshIV(cfg, p)
	printPlan(p, cfg)

	allowed, reason := appSettings.CanExecuteLive(opts.live, cfg.Execution.KillSwitch)
	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	// 実行品質の記録は最後にまとめて書き出す（1 発注 1 ファイルにしない）
	defer func() {
		if err := execution.Flush(historyStore(), day); err != nil {
			logWarn("daytrade.execution", "実行品質の記録に失敗", map[string]any{"error": err.Error()})
		}
	}()

	if !allowed {
		// dry-run は確認のたびに増える。その日の古い dry-run は消して最新だけ残す
		if _, err := led.ClearDryRun(day); err != nil {
			return err
		}
	}
	// 生きている／約定した建玉があれば今日は終わり。拒否・失効だけなら送り直す
	entries, err := led.EntriesOn(day)
	if err != nil {
		return err
	}
	var already int
	for _, o := range entries {
		if !o.IsDryRun() && !o.IsDead() {
			already++
		}
	}
	if already > 0 {
		fmt.Printf("今日の建玉は発注済み（%d 件、冪等）。何もしません\n", already)
		logInfo("daytrade.skip", "発注済み", map[string]any{"reason": "already", "orders": already})
		return nil
	}

	eligible := p.Eligible()
	symbols := p.Symbols(eligible)
	shortUniverse := p.ShortEligible()
	if cfg.Margin.Enabled && !watchOnly {
		// ショートの母集団はロングと別なので、気配はその和集合で取る
		symbols = mergeSymbols(symbols, p.Symbols(shortUniverse))
	}

	received, err := fetchQuotes(cfg, symbols, opts.quoteSource, opts.quoteFile)
	if err != nil {
		fmt.Println(err)
		logError("daytrade.skip", "気配が取れず寄付の買いを見送り", map[string]any{"reason": "no_quotes", "error": err.Error()})
		alert("デイトレ: 気配が取れず寄付の買いを見送り", fmt.Sprintf("%s 候補 %d 銘柄: %v", day.Format(DateLayout), len(symbols), err))
		digest.Anomaly("daytrade.no_quotes", err.Error())
		return nil
	}
	quotes, stale, delayed := dtquotes.Fresh(received, cfg.Execution.MaxQuoteAge, now, opts.allowDelayed)
	if len(stale) > 0 || len(delayed) > 0 {
		logWarn("daytrade.quotes", "使えない気配を除外", map[string]any{
			"stale": len(stale), "stale_sample": sample(stale),
			"delayed": len(delayed), "delayed_sample": sample(delayed),
			"max_age_sec": cfg.Execution.MaxQuoteAge,
		})
	}

	prevAll := p.PrevCloseBySymbol()
	appendHistory(dthistory.KindQuotes, dthistory.QuotesFrame(received, quotes, prevAll), day)

	summary := map[string]any{
		"mode":             modeOf(watchOnly, allowed),
		"quotes_requested": len(symbols),
		"quotes_received":  len(received),
		"quotes_usable":    len(quotes),
	}
	finish := func(outcome string, extra map[string]any) {
		row := map[string]any{}
		for k, v := range summary {
			row[k] = v
		}
		for k, v := range extra {
			row[k] = v
		}
		row["outcome"] = outcome
		appendHistory(dthistory.KindOpenRun, dthistory.OpenRunFrame(row), day)
	}

	if len(quotes) == 0 {
		fmt.Println("使える気配がありません。発注しません")
		logError("daytrade.skip", "気配が無いため見送り", map[string]any{"reason": "no_quotes"})
		alert("デイトレ: 気配が取れず寄付の買いを見送り",
			fmt.Sprintf("%s 候補 %d 銘柄", day.Format(DateLayout), len(symbols)))
		finish("no_quotes", nil)
		return nil
	}

	// 市場ギャップは候補全体の中央値で代用する（TOPIX の寄付は取れない）
	var gaps []float64
	for symbol, q := range quotes {
		if prev, ok := prevAll[symbol]; ok && prev > 0 {
			price, _ := q.Price.Float64()
			gaps = append(gaps, price/prev-1)
		}
	}
	verdict, err := evaluateRegime(cfg, p, day, regime.MarketGapOf(gaps), led)
	if err != nil {
		return err
	}
	summary["trade"] = verdict.Trade
	summary["reasons"] = strings.Join(verdict.Reasons, "、")
	summary["scale"] = verdict.Scale
	for k, v := range verdict.Notes {
		summary[k] = v
	}
	if !verdict.Trade {
		fmt.Println("危険信号により今日は取引しません: " + strings.Join(verdict.Reasons, "、"))
		logInfo("daytrade.skip", "危険信号で見送り", map[string]any{"reason": "regime", "reasons": verdict.Reasons})
		digest.Note(map[string]any{"regime_skip": strings.Join(verdict.Reasons, "、")})
		finish("regime", nil)
		return nil
	}

	// 様子見モードでは「買うとしたら」の上位を目安の予算で見せる
	n := cfg.Capital.Positions()
	budget := cfg.Capital.BudgetPerOrder()
	weighting := cfg.Capital.Weighting
	if watchOnly {
		n = watchRows
		budget = cfg.Capital.OrderBudget
		weighting = "equal"
	}
	weak := verdict.Weak()
	if weak && (!cfg.Margin.Enabled || cfg.Margin.LongShrink) {
		budget = budget.Mul(decimal.NewFromFloat(verdict.Scale)).Round(0)
		fmt.Printf("%s（1 注文 %s 円）\n", verdict.ScaleReason, yen(budget))
	} else if weak {
		fmt.Println(strings.Split(verdict.ScaleReason, "→")[0] +
			"→ ロングは縮めず、ショートを建てる合図にする")
	}

	ranking := selection.Rank(eligible, quotes, cfg.Signal)
	picks := selection.PickFrom(ranking, selection.PickOptions{
		N: n, Budget: budget, Weighting: weighting, Side: domain.SideBuy,
	})
	longPicks := len(picks)
	frames := []history.Frame{dthistory.RankingFrame(ranking, picks, "BUY", n, budget)}
	summary["n"], summary["budget"], summary["weighting"], summary["weak"] = n, budget, weighting, weak
	printPicks(picks, len(quotes), p, watchOnly, "")
	logRanking(day, "BUY", ranking, picks, n, budget, verdict.Scale, weighting, len(quotes))

	// ショートの脚（[margin]）: 貸借銘柄 × ギャップ上位を売建てる。資金はシーソー
	shortMultiplier := decimal.Zero
	if cfg.Margin.Enabled && cfg.Margin.Positions() > 0 && !watchOnly {
		shortMultiplier = cfg.Margin.MultiplierNormal
		if weak {
			shortMultiplier = cfg.Margin.MultiplierLongWeak
		}
	}
	if shortMultiplier.GreaterThan(decimal.Zero) {
		shortN := cfg.Margin.Positions()
		shortBudget := cfg.Margin.BudgetPerOrder().Mul(shortMultiplier).Round(0)
		shortRanking := selection.RankShort(shortUniverse, quotes, cfg.Margin)
		shortPicks := selection.PickFrom(shortRanking, selection.PickOptions{
			N: shortN, Budget: shortBudget, Weighting: cfg.Margin.Weighting, Side: domain.SideSell,
		})
		label := "通常日"
		if weak {
			label = "弱い日"
		}
		fmt.Printf("ショート: %sの倍率 %s × 1 注文 %s 円 = %s 円  対象 %d 銘柄\n",
			label, shortMultiplier.String(), yen(cfg.Margin.BudgetPerOrder()), yen(shortBudget), len(shortUniverse))
		printPicks(shortPicks, len(quotes), p, false, "寄付の売建（信用）")
		frames = append(frames, dthistory.RankingFrame(shortRanking, shortPicks, "SELL", shortN, shortBudget))
		summary["short_n"] = shortN
		summary["short_budget"] = shortBudget
		summary["short_multiplier"] = shortMultiplier
		logRanking(day, "SELL", shortRanking, shortPicks, shortN, shortBudget, verdict.Scale, cfg.Margin.Weighting, len(quotes))
		picks = append(picks, shortPicks...)
	} else if cfg.Margin.Enabled && !watchOnly {
		fmt.Println("ショート: この日は建てない（倍率 0）")
	}
	if path := appendHistory(dthistory.KindRanking, concatFrames(frames), day); path != "" {
		fmt.Printf("履歴に追記 %s\n", path)
	}
	summary["long_picks"] = longPicks
	summary["short_picks"] = len(picks) - longPicks

	if len(picks) == 0 {
		logInfo("daytrade.skip", "条件に合う銘柄なし", map[string]any{"reason": "no_picks", "quotes": len(quotes)})
		finish("no_picks", nil)
		return nil
	}
	if watchOnly {
		logInfo("daytrade.skip", "資金 0 のため買わない", map[string]any{"reason": "no_capital", "picks": len(picks)})
		finish("no_capital", nil)
		return nil
	}
	if err := confirmLive(allowed, opts.yes); err != nil {
		return err
	}

	// 台帳に無い建玉がブローカーにあれば、この実行は二重に建てることになる。
	//
	// 冪等性は台帳の client_order_id で担保しているので、台帳を失う・別ホストへ
	// 移す・復元した直後は効かない。ブローカー側の建玉は失われないので、発注の
	// 直前に突き合わせる（積立の UnrecordedFills と同じ考え）。
	if allowed {
		if err := ensureNoUnrecordedPositions(cfg, led, day, picks); err != nil {
			return err
		}
	}

	orders, failures, err := placePicks(cfg, led, picks, day, allowed)
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		alert(fmt.Sprintf("デイトレ: %d 件の建玉が通らず", len(failures)), strings.Join(failures, "\n"))
		digest.Anomaly("daytrade.order_failed", fmt.Sprintf("%d 件の建玉が通らず", len(failures)))
	}
	finish("picked", map[string]any{"orders": orders, "failures": len(failures)})
	logInfo("daytrade.run", "寄付の買いを終了", map[string]any{
		"phase": "open", "live": allowed, "reason": reason,
		"n": n, "budget": budget.String(), "scale": verdict.Scale,
		"picks": len(picks), "failures": len(failures),
	})
	digest.Note(map[string]any{
		"phase": "open", "live": allowed, "picks": len(picks), "failures": len(failures),
	})
	return nil
}

// placePicks は選んだ銘柄を順に発注する。台帳への記録が先（placeRecorded）。
func placePicks(cfg dtconfig.Config, led *dtledger.Ledger, picks []selection.Pick, day time.Time, allowed bool) (orders int, failures []string, err error) {
	var b broker.Broker
	if allowed {
		if b, err = connectBroker(cfg); err != nil {
			return 0, nil, err
		}
	}
	// 余力は取引区分ごと（現物は買付余力、信用は新規建可能額）。同じ枠を使う注文で減らしていく
	remaining := map[domain.TradeType]decimal.Decimal{}

	for _, pick := range picks {
		attempt := led.DeadCount(day, pick.Symbol, pick.Side)
		request := entryRequest(pick, day, cfg, attempt)
		label := "買い"
		if pick.Side == domain.SideSell {
			label = "売建"
		}
		if led.WasPlaced(request.ClientOrderID) {
			fmt.Printf("  %s: %sは発注済み（冪等）\n", pick.Symbol, label)
			skipRow(pick, request, execution.ReasonIdempotent, "")
			continue
		}
		outcome := ""
		if b == nil {
			if err := led.Record(request, day, dtledger.DryRunStatus, &pick.Price, nil); err != nil {
				return orders, failures, err
			}
			skipRow(pick, request, execution.ReasonDryRun, "")
			outcome = "dry-run"
			orders++
		} else {
			if _, ok := remaining[request.Trade]; !ok {
				balance, err := b.GetBalance()
				if err != nil {
					return orders, failures, err
				}
				remaining[request.Trade] = balance.BuyingPowerFor(request.Trade)
			}
			need := pick.Amount().Add(pick.Fee())
			if need.GreaterThan(remaining[request.Trade]) {
				outcome = fmt.Sprintf("見送り 余力不足（必要 %s / 余力 %s）", yen(need), yen(remaining[request.Trade]))
				failures = append(failures, fmt.Sprintf("%s %s: %s", pick.Symbol, label, outcome))
				fmt.Printf("  %s: %s\n", pick.Symbol, outcome)
				skipRow(pick, request, execution.ReasonInsufficientFunds, outcome)
				continue
			}
			remaining[request.Trade] = remaining[request.Trade].Sub(need)
			fee := pick.Fee()
			if err := placeRecorded(b, led, request, day, pick.Price, &fee); err != nil {
				outcome = fmt.Sprintf("失敗 %v", err)
				failures = append(failures, fmt.Sprintf("%s %s: %v", pick.Symbol, label, err))
				fmt.Printf("  %s: %s\n", pick.Symbol, outcome)
			} else {
				outcome = "発注"
				orders++
			}
		}
		logInfo("daytrade.order", "寄付の注文", map[string]any{
			"day": day.Format(DateLayout), "symbol": pick.Symbol,
			"side": string(pick.Side), "trade": string(request.Trade),
			"client_order_id": request.ClientOrderID,
			"quantity":        pick.Quantity.String(), "price": pick.Price.String(),
			"amount": pick.Amount().String(), "live": b != nil, "outcome": outcome,
		})
	}
	return orders, failures, nil
}

// entryRequest は建てる注文。ロング（BUY）は現物か信用買い、ショート（SELL）は信用新規売り。
func entryRequest(pick selection.Pick, day time.Time, cfg dtconfig.Config, attempt int) domain.OrderRequest {
	// 前回が拒否されていたら種を変える（同じ ID はブローカーが弾く）。attempt 0 は従来と同じ ID
	seed := "daytrade|" + day.Format(DateLayout)
	if attempt > 0 {
		seed = fmt.Sprintf("%s|%d", seed, attempt)
	}
	trade := domain.TradeTypeCash
	action := "買い"
	switch {
	case pick.Side == domain.SideSell:
		trade = domain.TradeTypeMarginOpen // 売建（空売り）
		action = "売建"
	case cfg.Margin.Enabled && cfg.Margin.LongViaMargin:
		trade = domain.TradeTypeMarginOpen // 信用買い（日計り。手数料 0 円）
	}
	gap, _ := pick.Gap.Float64()
	return domain.OrderRequest{
		ClientOrderID: domain.MakeClientOrderID(seed, pick.Symbol, pick.Side, pick.Quantity),
		Symbol:        pick.Symbol,
		Side:          pick.Side,
		OrderType:     domain.OrderTypeMarket,
		Quantity:      pick.Quantity,
		TaxType:       cfg.Execution.TaxAccountType,
		Reason: fmt.Sprintf("%s %s gap %s #%d %s",
			cfg.StrategyName(), day.Format(DateLayout), pct(gap), pick.Rank, action),
		Trade: trade,
	}
}

// ErrUnconfirmedOrder は送信したが結果を確認できなかった注文。
//
// 通信断やタイムアウトでは「届いていない」と「届いたが応答が返らない」を区別できない。
// 台帳には送信中（PENDING）が残るので、次の実行は WasPlaced で弾かれて再送されない。
type ErrUnconfirmedOrder struct {
	ClientOrderID string
	Err           error
}

func (e *ErrUnconfirmedOrder) Error() string {
	return fmt.Sprintf("注文 %s の結果を確認できませんでした（送信済みの可能性があります）: %v",
		e.ClientOrderID, e.Err)
}

func (e *ErrUnconfirmedOrder) Unwrap() error { return e.Err }

// placeRecorded は送る前に台帳へ PENDING を書き、送ったら結果で更新する。
//
// 送信後に落ちても台帳には残るので、次の実行で同じ注文を送り直さない
// （二重買付より買い漏れの方がまし）。
//
//   - 受理された           → その状態で上書き
//   - 明確に拒否された     → REJECTED。PENDING のままだと「送信結果不明」と区別できない
//   - それ以外（通信断等） → 送信中のまま残し ErrUnconfirmedOrder
//
// 同時に実行品質の intent 行を残す。台帳の price は後で約定額に上書きされうるので、
// 判断時の想定はここで別に控えておく。
func placeRecorded(b broker.Broker, led *dtledger.Ledger, request domain.OrderRequest, day time.Time, price decimal.Decimal, fee *decimal.Decimal) error {
	intent := func(reason execution.ReasonCode, note string) {
		execution.Collect(execution.Spec{
			Event: execution.EventIntent, App: "daytrade",
			Symbol: request.Symbol, Side: string(request.Side), Trade: string(request.Trade),
			ClientOrderID: request.ClientOrderID, Live: true,
			Quantity:     request.Quantity,
			IntentPrice:  price,
			IntentAmount: price.Mul(request.Quantity),
			IntentFee:    fee,
			Reason:       reason, Note: note,
		})
	}
	if err := led.Record(request, day, string(domain.OrderStatusPending), &price, nil); err != nil {
		return fmt.Errorf("発注前の台帳記録に失敗しました（発注を中止します）: %w", err)
	}
	ack, err := b.Place(request)
	if err != nil {
		var rejected *broker.OrderRejectedError
		if errors.As(err, &rejected) {
			_ = led.UpdateStatus(request.ClientOrderID, domain.OrderStatusRejected, decimal.Zero, nil, nil)
			intent(execution.ReasonBrokerError, err.Error())
			return err
		}
		intent(execution.ReasonUnconfirmed, err.Error())
		return &ErrUnconfirmedOrder{ClientOrderID: request.ClientOrderID, Err: err}
	}
	_ = led.UpdateStatus(request.ClientOrderID, ack.Status, decimal.Zero, nil, ack.BrokerOrderID)
	intent(execution.ReasonPlaced, "")
	return nil
}

// skipRow は発注しなかった建玉を実行品質に残す。
// **見送りの理由の分布が改善の材料になる。**
func skipRow(pick selection.Pick, request domain.OrderRequest, reason execution.ReasonCode, note string) {
	fee := pick.Fee()
	execution.Collect(execution.Spec{
		Event: execution.EventSkip, App: "daytrade",
		Symbol: pick.Symbol, Side: string(request.Side), Trade: string(request.Trade),
		ClientOrderID: request.ClientOrderID, Live: false,
		Quantity: pick.Quantity, IntentPrice: pick.Price,
		IntentAmount: pick.Amount(), IntentFee: fee,
		Reason: reason, Note: note,
	})
}

// evaluateRegime は危険信号を評価し、ログに残す。
func evaluateRegime(cfg dtconfig.Config, p dtplan.Plan, day time.Time, marketGap *float64, led *dtledger.Ledger) (regime.Verdict, error) {
	signals := regime.Signals{
		Day:       day,
		IVPrev:    p.Meta.IVPrev,
		Drift:     p.Meta.Drift,
		MarketGap: marketGap,
	}
	if cfg.Regime.EquityCurveDays > 0 {
		recent, err := recentPnL(cfg, day, led)
		if err != nil {
			return regime.Verdict{}, err
		}
		signals.RecentPnL = recent
	}
	if cfg.Regime.UsSkipHigh != nil {
		session, err := usmarketLatest(day)
		if err != nil {
			// 取得元の障害で寄付の判断を止めない
			logWarn("daytrade.us_missing", "米国市場の取得に失敗", map[string]any{"error": err.Error()})
		} else if session != nil {
			signals.UsRet = &session.SpxRet
			// VIX は FRED の公開が S&P500 より 1 日遅れることがある。取れていない日に
			// 0 を渡すと「VIX が低い」と読まれるので、無いものは無いままにする
			if session.Vix > 0 {
				signals.Vix = &session.Vix
			}
		}
	}
	verdict := regime.Evaluate(cfg.Regime, signals)
	fields := map[string]any{"day": day.Format(DateLayout), "trade": verdict.Trade, "reasons": verdict.Reasons}
	for k, v := range verdict.Notes {
		fields[k] = v
	}
	logInfo("daytrade.regime", "危険信号", fields)
	return verdict, nil
}

func modeOf(watchOnly, allowed bool) string {
	switch {
	case watchOnly:
		return "watch"
	case allowed:
		return "live"
	default:
		return "dry_run"
	}
}

func mergeSymbols(a, b []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sortStrings(out)
	return out
}

func sample(values []string) []string {
	if len(values) > sampleSize {
		return values[:sampleSize]
	}
	return values
}

// concatFrames は同じ形の表を縦に結合する。
func concatFrames(frames []history.Frame) history.Frame {
	if len(frames) == 0 {
		return history.Frame{}
	}
	out := frames[0]
	for _, f := range frames[1:] {
		out.Rows = append(out.Rows, f.Rows...)
	}
	return out
}

func logRanking(day time.Time, side string, ranking []selection.Ranked, picks []selection.Pick, n int, budget decimal.Decimal, scale float64, weighting string, quotes int) {
	picked := map[string]selection.Pick{}
	for _, p := range picks {
		picked[p.Symbol] = p
	}
	limit := min(len(ranking), n+rankingExtra)
	rows := make([]map[string]any, 0, limit)
	for _, r := range ranking[:limit] {
		row := map[string]any{
			"rank": r.Rank, "symbol": r.Symbol, "gap": r.Gap.String(), "price": r.Price.String(),
			"picked": false,
		}
		if r.Vol != nil {
			row["vol"] = *r.Vol
		}
		if p, ok := picked[r.Symbol]; ok {
			row["picked"] = true
			row["quantity"] = p.Quantity.String()
		}
		rows = append(rows, row)
	}
	logInfo("daytrade.ranking", "順位表（N と次点）", map[string]any{
		"day": day.Format(DateLayout), "side": side, "n": n, "budget": budget.String(),
		"scale": scale, "weighting": weighting, "quotes": quotes, "rows": rows,
	})
}

// printPicks は選んだ銘柄の表。
func printPicks(picks []selection.Pick, quotes int, p dtplan.Plan, watchOnly bool, label string) {
	if label == "" {
		label = "寄付の買い"
		if watchOnly {
			label = "候補（買わない: 資金 0）"
		}
	}
	fmt.Printf("\n%s %s（気配 %d 銘柄から）\n", p.Meta.Day, label, quotes)
	if len(picks) == 0 {
		fmt.Println("  条件に合う銘柄がありません（条件のギャップが無いか、1 単元が予算に届かない）")
		return
	}
	fmt.Printf("  %-3s %-6s %-12s %-4s %10s %10s %8s %8s %12s %8s\n",
		"#", "銘柄", "名称", "売買", "前日終値", "気配", "ギャップ", "株数", "金額", "手数料")
	for _, pick := range picks {
		action := "買い"
		if pick.Side == domain.SideSell {
			action = "売建"
		}
		gap, _ := pick.Gap.Float64()
		fmt.Printf("  %-3d %-6s %-12s %-4s %10s %10s %8s %8s %12s %8s\n",
			pick.Rank, pick.Symbol, truncate(pick.Name, 12), action,
			yen(pick.PrevClose), yen(pick.Price), pct(gap),
			yen(pick.Quantity), yen(pick.Amount()), yen(pick.Fee()))
	}
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// fetchQuotes は設定（または上書き）の取得元から気配を取る。
func fetchQuotes(cfg dtconfig.Config, symbols []string, sourceOverride, fileOverride string) (map[string]selection.Quote, error) {
	name := cfg.Execution.QuoteSource
	if sourceOverride != "" {
		name = sourceOverride
	}
	file := cfg.Execution.QuoteFile
	if fileOverride != "" {
		file = fileOverride
	}
	source, err := dtquotes.New(name, dtquotes.Params{
		Env: appSettings.Env, Dotenv: appSettings.DotenvMap,
		StateDir: appSettings.StateDir, QuoteFile: file,
	})
	if err != nil {
		return nil, err
	}
	found, err := source.Fetch(symbols)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, s := range symbols {
		if _, ok := found[s]; !ok {
			missing = append(missing, s)
		}
	}
	logInfo("daytrade.quotes", "気配を取得", map[string]any{
		"source": name, "requested": len(symbols), "received": len(found),
		"missing": len(missing), "missing_sample": sample(missing),
	})
	return found, nil
}

// ensureNoUnrecordedPositions は、これから建てる銘柄に台帳外の建玉が無いか確かめる。
//
// 台帳（client_order_id）に基づく冪等性は台帳が生きている前提の仕組みで、失うと
// 効かない。ブローカー側の建玉は失われないので、発注の直前にそちらと突き合わせる。
// 見つかったら**発注を中止する**——自動で手仕舞うと他の戦略の保有を触りかねないし、
// 二重に建てると日計りの資金計画が崩れる。人が確かめるまで止める。
//
// 照会できないときも中止する。二重建てを否定できないまま実弾を出さない。
func ensureNoUnrecordedPositions(
	cfg dtconfig.Config,
	led *dtledger.Ledger,
	day time.Time,
	picks []selection.Pick,
) error {
	if len(picks) == 0 {
		return nil
	}
	b, err := connectBroker(cfg)
	if err != nil {
		return fmt.Errorf("建玉を突き合わせられないため発注を中止しました: %w", err)
	}
	positions, err := broker.PositionsBySymbolIncludingMargin(b)
	if err != nil {
		logError("daytrade.unrecorded_check_failed", "建玉を照会できません",
			map[string]any{"day": day.Format(DateLayout), "error": err.Error()})
		return fmt.Errorf("建玉を照会できないため発注を中止しました（二重に建てないため）: %w", err)
	}

	// 台帳が知っている今日の建玉ぶんは差し引く（正常な再実行では止めない）
	recorded := map[string]decimal.Decimal{}
	if entries, err := led.EntriesOn(day); err == nil {
		for _, o := range entries {
			if o.IsDryRun() || o.IsDead() {
				continue
			}
			sign := decimal.NewFromInt(1)
			if o.Side != domain.SideBuy {
				sign = decimal.NewFromInt(-1)
			}
			recorded[o.Symbol] = recorded[o.Symbol].Add(sign.Mul(o.Quantity))
		}
	}

	var unrecorded []string
	seen := map[string]struct{}{}
	for _, pick := range picks {
		if _, done := seen[pick.Symbol]; done {
			continue
		}
		seen[pick.Symbol] = struct{}{}
		held, ok := positions[pick.Symbol]
		if !ok || held.Quantity.IsZero() {
			continue
		}
		if leftover := held.Quantity.Sub(recorded[pick.Symbol]); !leftover.IsZero() {
			unrecorded = append(unrecorded, fmt.Sprintf("%s %s 株（台帳の記録は %s 株）",
				pick.Symbol, leftover, recorded[pick.Symbol]))
		}
	}
	if len(unrecorded) == 0 {
		return nil
	}

	sortStrings(unrecorded)
	message := strings.Join(unrecorded, "、")
	logError("daytrade.unrecorded_positions", "台帳に無い建玉があります",
		map[string]any{"day": day.Format(DateLayout), "positions": unrecorded})
	digest.Anomaly("daytrade.unrecorded_positions",
		fmt.Sprintf("%d 銘柄に台帳外の建玉", len(unrecorded)))
	alert("デイトレ: 台帳に無い建玉があります（二重に建てる恐れ）。発注を中止しました", message)
	return fmt.Errorf("台帳に無い建玉があります（二重に建てます）: %s\n"+
		"台帳（%s）が失われているか、別の環境で発注した可能性があります。"+
		"口座を確かめてから実行してください", message, led.Path())
}
