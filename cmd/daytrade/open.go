package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
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
		brokerVerifyFlag bool
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
				quoteFile: quoteFileFlag, date: dateFlag, brokerVerify: brokerVerifyFlag,
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
	cmd.Flags().BoolVar(&brokerVerifyFlag, "broker-verify", false,
		"発注経路の実機検証（docs/BROKER_VERIFY.md）。台帳・履歴・ログに印を付け、成績の集計から外す")
	return cmd
}

type openOptions struct {
	live, yes, ignoreWindow, allowDelayed bool
	quoteSource, quoteFile, date          string
	// brokerVerify は実機検証の実行か（--broker-verify）。建てる玉は本物だが、
	// 戦略の判断ではないので成績の集計からは外す。
	brokerVerify bool
}

func runOpen(opts openOptions) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// 以降のログの全行とダイジェストに印を付ける（env とは独立）
	run.SetVerify(opts.brokerVerify)
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
	// 締め切り: 時間帯の終わり（live のとき）と開始 + max_run_seconds の早い方。
	// 過ぎたら新しい電文は送らず、送信中は打ち切る。遅い日に 1 回がロックを握り続けて
	// 次の cron まで潰さないため（送れなかった分は次の回が残りの枚数として建て直す）
	started := now
	deadline := cfg.Execution.RunDeadline("entry", now, opts.live && !opts.ignoreWindow, jst)
	logConfig(cfg, "open", map[string]any{
		"day": day.Format(DateLayout), "live": opts.live,
		"allow_delayed": opts.allowDelayed, "quote_override": opts.quoteSource,
		"watch_only": watchOnly, "deadline": deadlineText(deadline),
		"max_run_seconds": cfg.Execution.MaxRunSeconds,
		"broker_verify":   opts.brokerVerify,
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
	led.Verify = opts.brokerVerify
	defer led.Close()
	// 実行品質の記録は最後にまとめて書き出す（1 発注 1 ファイルにしない）
	defer func() {
		if err := execution.Flush(historyStore(), day); err != nil {
			logWarn("daytrade.execution", "実行品質の記録に失敗", map[string]any{"error": err.Error()})
		}
	}()

	env := execute.Env{
		Cfg: cfg, Ledger: led, Day: day, Report: run, Out: os.Stdout,
		RetryWait: execute.DefaultRetryWait, Deadline: deadline,
	}
	var b broker.Broker
	var carried []execute.Carried
	if allowed {
		if b, err = connectBroker(cfg); err != nil {
			return err
		}
		broker.SetDeadline(b, deadline)
		// 前回の実行で送信結果が分からなかった注文があれば、ここで判定して台帳を直す。
		// 届いていなければ UNSENT になり、下の「発注済み」には数えない（種を変えて送り直す）
		if err := resolvePending(env, b); err != nil {
			return err
		}
		// 前営業日以前の建玉が残っていれば（引けで返済できなかった持ち越し）、新規に建てる前に
		// 寄付の成行で手仕舞う。検証は margin.carry_penalty で「翌寄りで返済」としているので同じにする。
		// 判定できなければ止める——持ち越しを知らずに建てると二重になりうる
		if carried, err = settleCarried(env, b, "翌寄りで持ち越しを手仕舞い"); err != nil {
			return err
		}
	} else {
		// dry-run は確認のたびに増える。その日の古い dry-run は消して最新だけ残す
		if _, err := led.ClearDryRun(day); err != nil {
			return err
		}
	}
	// 生きている／約定した建玉の数。再実行は「N − これ」だけを建てる——1 回目が途中で
	// 落ちても（通信エラー・締め切り）、次の cron が残りを埋める。拒否・失効は数えない
	placed, err := execute.PlacedToday(env)
	if err != nil {
		return err
	}
	remainingLong, remainingShort := cfg.Capital.Positions()-placed.Long, 0
	if cfg.Margin.Enabled && !watchOnly {
		remainingShort = cfg.Margin.Positions() - placed.Short
	}
	// 持ち越しが拘束している資金（残り株数 × 建値）を上限から引き、残りで建てられる件数に頭打ち。
	// 返済注文は出したが、寄っていない銘柄はまだ約定しておらず資金は戻っていない
	if tiedLong, tiedShort := execute.TiedCapital(carried); tiedLong.IsPositive() || tiedShort.IsPositive() {
		capLong := execute.PositionsWithin(cfg.Capital.MaxCapital, tiedLong, cfg.Capital.OrderBudget)
		capShort := execute.PositionsWithin(cfg.Margin.MaxCapital, tiedShort, cfg.Margin.OrderBudget)
		remainingLong, remainingShort = min(remainingLong, capLong), min(remainingShort, capShort)
		fmt.Printf("持ち越しが拘束する資金 ロング %s 円 / ショート %s 円 → 今日はロング %d 件 / ショート %d 件まで\n",
			yen(tiedLong), yen(tiedShort), max(remainingLong, 0), max(remainingShort, 0))
		logWarn("daytrade.carry", "持ち越しの拘束資金で件数を頭打ち", map[string]any{
			"tied_long": tiedLong.String(), "tied_short": tiedShort.String(),
			"cap_long": capLong, "cap_short": capShort,
			"remaining_long": max(remainingLong, 0), "remaining_short": max(remainingShort, 0),
		})
	}
	if placed.Total() > 0 {
		if remainingLong <= 0 && remainingShort <= 0 {
			fmt.Printf("今日の建玉は発注済み（ロング %d / ショート %d 件、冪等）。何もしません\n", placed.Long, placed.Short)
			logInfo("daytrade.skip", "発注済み", map[string]any{
				"reason": "already", "orders": placed.Total(), "long": placed.Long, "short": placed.Short})
			return nil
		}
		fmt.Printf("今日は既にロング %d / ショート %d 件を建てています。残り（ロング %d / ショート %d）だけ建てます\n",
			placed.Long, placed.Short, max(remainingLong, 0), max(remainingShort, 0))
		logInfo("daytrade.resume", "建玉の残りを建て直す", map[string]any{
			"long": placed.Long, "short": placed.Short,
			"remaining_long": max(remainingLong, 0), "remaining_short": max(remainingShort, 0),
			"symbols": sortedKeys(placed.Symbols),
		})
	}

	eligible := p.Eligible()
	symbols := p.Symbols(eligible)
	shortUniverse := p.ShortEligible()
	if cfg.Margin.Enabled && !watchOnly {
		// ショートの母集団はロングと別なので、気配はその和集合で取る
		symbols = mergeSymbols(symbols, p.Symbols(shortUniverse))
	}

	received, err := fetchQuotes(cfg, symbols, opts.quoteSource, opts.quoteFile, deadline)
	if err != nil {
		fmt.Println(err)
		logError("daytrade.skip", "気配が取れず寄付の買いを見送り", map[string]any{"reason": "no_quotes", "error": err.Error()})
		alert("デイトレ: 気配が取れず寄付の買いを見送り", fmt.Sprintf("%s 候補 %d 銘柄: %v", day.Format(DateLayout), len(symbols), err))
		digest.Anomaly("daytrade.no_quotes", err.Error())
		return nil
	}
	quotes, stale, delayed := dtquotes.Fresh(received, cfg.Execution.MaxQuoteAge, now, opts.allowDelayed)
	// 気配の時刻（tDPP:T）には今日の日付を当てている。寄り前の銘柄で前日の時刻が返ると
	// 「今日の 15:30」＝未来として鮮度の検査を素通りする。実機で確かめるまで、除外した
	// 銘柄の時刻と年齢、未来の時刻を持つ銘柄の数を残す（docs/OPENING_DATA.md「実機で確かめること」）
	future := dtquotes.FutureStamped(received, now, futureSlack)
	if len(stale) > 0 || len(delayed) > 0 || len(future) > 0 {
		logWarn("daytrade.quotes", "使えない気配を除外", map[string]any{
			"stale": len(stale), "stale_sample": dtquotes.DescribeAges(received, sample(stale), now),
			"delayed": len(delayed), "delayed_sample": sample(delayed),
			"future": len(future), "future_sample": dtquotes.DescribeAges(received, sample(future), now),
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
		// signal.skip_opened で外した「既に寄っていた」銘柄の数（設定が偽なら null）
		"quotes_opened": nil,
		// margin.spill_to_long でロングに回したショートの余り（円。回さなかった日は null）
		"spill":         nil,
		"already_long":  placed.Long,
		"already_short": placed.Short,
		"deadline":      deadlineText(deadline),
		"broker_verify": opts.brokerVerify,
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
		row["elapsed_ms"] = clock.NowUTC().Sub(started).Milliseconds()
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
	n := max(remainingLong, 0)
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
	// ショック日（regime.shock_*）: 縮小の後にロングの予算へ倍率を掛ける（検証と同じ順序）
	if verdict.Shock && !watchOnly {
		budget = budget.Mul(decimal.NewFromFloat(verdict.ShockLong)).Round(0)
		fmt.Printf("%s（ロング 1 注文 %s 円）\n", verdict.ShockReason, yen(budget))
		logInfo("daytrade.regime", "ショック日", map[string]any{
			"reason": verdict.ShockReason, "long_scale": verdict.ShockLong, "short_scale": verdict.ShockShort,
		})
	}

	// signal.skip_opened: 9:01 の時点で既に寄っている銘柄を候補から外す。
	// 順位付けの直前に気配そのものを落とすので、ロング・ショートの両方に効く
	// （市場ギャップと危険信号は落とす前の気配で見る——候補全体の分布が変わるため）
	rankQuotes := quotes
	if len(placed.Symbols) > 0 {
		// 同じ日に建てた銘柄は重ねて建てない（再実行は残りの枚数を別の銘柄で埋める）
		rankQuotes = dtquotes.DropSymbols(rankQuotes, placed.Symbols)
	}
	if cfg.Signal.SkipOpened {
		kept, dropped := dtquotes.DropOpened(quotes)
		rankQuotes = kept
		summary["quotes_opened"] = int64(len(dropped))
		fmt.Printf("既に寄っている %d 銘柄を候補から外しました（signal.skip_opened。残り %d）\n",
			len(dropped), len(rankQuotes))
		logInfo("daytrade.quotes", "既に寄っている銘柄を除外", map[string]any{
			"opened": len(dropped), "opened_sample": sample(dropped), "remaining": len(rankQuotes),
		})
	}

	// ショートの脚（[margin]）を**先に**決める: 使わなかった資金をロングに回すため
	// （margin.spill_to_long。検証の simulateMarginSpill と同じ順序）。資金はシーソー
	shortMultiplier := decimal.Zero
	if cfg.Margin.Enabled && cfg.Margin.Positions() > 0 && !watchOnly {
		shortMultiplier = cfg.Margin.MultiplierNormal
		if weak {
			shortMultiplier = cfg.Margin.MultiplierLongWeak
		}
		if verdict.Shock {
			shortMultiplier = shortMultiplier.Mul(decimal.NewFromFloat(verdict.ShockShort))
		}
	}
	var (
		shortRanking []selection.Ranked
		shortPicks   []selection.Pick
		shortN       int
		shortBudget  decimal.Decimal
	)
	if shortMultiplier.GreaterThan(decimal.Zero) && remainingShort > 0 {
		shortN = remainingShort
		shortBudget = cfg.Margin.BudgetPerOrder().Mul(shortMultiplier).Round(0)
		shortRanking = selection.RankShort(shortUniverse, rankQuotes, cfg.Margin)
		shortPicks = selection.PickFrom(shortRanking, selection.PickOptions{
			N: shortN, Budget: shortBudget, Weighting: cfg.Margin.Weighting, Side: domain.SideSell,
			MaxAmount: cfg.Margin.MaxOrder,
		})
	}

	// ショートの余り（候補が無い・上限で頭打ち）をロングに回す。銘柄数は総予算 ÷ 1 注文の
	// 予算（capital.max_positions が上限）。倍率 0 の日（ショック日）は回す元が無い
	if cfg.Margin.SpillToLong && shortMultiplier.GreaterThan(decimal.Zero) && !watchOnly {
		used := decimal.Zero
		for _, pk := range shortPicks {
			used = used.Add(pk.Amount())
		}
		if spill := shortBudget.Mul(decimal.NewFromInt(int64(shortN))).Sub(used); spill.GreaterThan(decimal.Zero) {
			n, budget = selection.SpillInto(n, budget, cfg.Capital.BudgetPerOrder(), spill, cfg.Capital.MaxPositions)
			fmt.Printf("ショートの余り %s 円をロングに回す → N=%d、1 注文 %s 円\n", yen(spill), n, yen(budget))
			summary["spill"] = spill
			logInfo("daytrade.regime", "ショートの余りをロングへ", map[string]any{
				"spill": spill.String(), "n": n, "budget": budget.String()})
		}
	}

	ranking := selection.Rank(eligible, rankQuotes, cfg.Signal)
	picks := selection.PickFrom(ranking, selection.PickOptions{
		N: n, Budget: budget, Weighting: weighting, Side: domain.SideBuy,
	})
	longPicks := len(picks)
	frames := []history.Frame{dthistory.RankingFrame(ranking, picks, "BUY", n, budget)}
	summary["n"], summary["budget"], summary["weighting"], summary["weak"] = n, budget, weighting, weak
	printPicks(picks, len(rankQuotes), p, watchOnly, "")
	logRanking(day, "BUY", ranking, picks, n, budget, verdict.Scale, weighting, len(rankQuotes))

	if shortMultiplier.GreaterThan(decimal.Zero) {
		label := "通常日"
		if weak {
			label = "弱い日"
		}
		fmt.Printf("ショート: %sの倍率 %s × 1 注文 %s 円 = %s 円  対象 %d 銘柄\n",
			label, shortMultiplier.String(), yen(cfg.Margin.BudgetPerOrder()), yen(shortBudget), len(shortUniverse))
		printPicks(shortPicks, len(rankQuotes), p, false, "寄付の売建（信用）")
		frames = append(frames, dthistory.RankingFrame(shortRanking, shortPicks, "SELL", shortN, shortBudget))
		summary["short_n"] = shortN
		summary["short_budget"] = shortBudget
		summary["short_multiplier"] = shortMultiplier
		logRanking(day, "SELL", shortRanking, shortPicks, shortN, shortBudget, verdict.Scale, cfg.Margin.Weighting, len(rankQuotes))
		picks = append(picks, shortPicks...)
	} else if cfg.Margin.Enabled && !watchOnly && remainingShort <= 0 && placed.Short > 0 {
		fmt.Printf("ショート: 発注済み（%d 件）\n", placed.Short)
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

	if allowed {
		// 台帳に無い建玉がブローカーにあれば、この実行は二重に建てることになる。
		// 冪等性は台帳の client_order_id で担保しているので、台帳を失う・別ホストへ
		// 移す・復元した直後は効かない。発注の直前にブローカーと突き合わせる
		if err := execute.EnsureNoUnrecordedPositions(env, b, picks, carried); err != nil {
			var unrecorded *execute.ErrUnrecordedPositions
			if errors.As(err, &unrecorded) {
				digest.Anomaly("daytrade.unrecorded_positions",
					fmt.Sprintf("%d 銘柄に台帳外の建玉", len(unrecorded.Positions)))
			}
			return err
		}
	}

	orders, failures, err := execute.PlacePicks(env, b, picks)
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
		"already_long": placed.Long, "already_short": placed.Short,
		"elapsed_ms": clock.NowUTC().Sub(started).Milliseconds(), "deadline": deadlineText(deadline),
	})
	digest.Note(map[string]any{
		"phase": "open", "live": allowed, "picks": len(picks), "failures": len(failures),
	})
	return nil
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
	if usmarketNeeded(cfg) {
		// 前夜の plan が温めたキャッシュを先に見る。取りに行くときも 1 本 8 秒まで——
		// 寄付の判断に FRED の遅さを持ち込まない（取れなければゲートは効かせない）
		session, source, err := usmarketLatest(day, usFetchTimeout)
		if err != nil {
			// 取得元の障害で寄付の判断を止めない
			logWarn("daytrade.us_missing", "米国市場の取得に失敗", map[string]any{"error": err.Error(), "source": source})
		}
		if session != nil {
			logInfo("daytrade.us_session", "前夜の米国市場", map[string]any{"source": source, "session": session.Describe()})
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

// usFetchTimeout は寄付の判断で FRED を待つ上限（1 リクエスト）。
const usFetchTimeout = 8 * time.Second

// futureSlack は気配の時刻が「未来」とみなす余裕（時計のずれぶん）。
const futureSlack = time.Minute

// deadlineText は締め切りの JST 表記（無ければ空）。
func deadlineText(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return clock.ToZone(deadline, jst).Format("15:04:05")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

// fetchQuotes は設定（または上書き）の取得元から気配を取る。deadline は立花の電文の締め切り。
func fetchQuotes(cfg dtconfig.Config, symbols []string, sourceOverride, fileOverride string, deadline time.Time) (map[string]selection.Quote, error) {
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
		Logger: run, Deadline: deadline,
	})
	if err != nil {
		return nil, err
	}
	started := clock.NowUTC()
	found, err := source.Fetch(symbols)
	elapsed := clock.NowUTC().Sub(started).Milliseconds()
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
		"missing": len(missing), "missing_sample": sample(missing), "elapsed_ms": elapsed,
	})
	return found, nil
}

// settleCarried は前営業日以前の持ち越しを判定し、成行で手仕舞う（open / close 共用）。
//
// 照会できなかった注文がある銘柄は手仕舞わず、人に知らせる。通らなかった返済も知らせる
// （次の実行が同じ判定でもう一度送る。台帳は建てた日の下に記録され、冪等）。
func settleCarried(env execute.Env, b broker.Broker, phrase string) ([]execute.Carried, error) {
	carried, unconfirmed, err := execute.CarriedPositions(env, b)
	if err != nil {
		return nil, err
	}
	if len(unconfirmed) > 0 {
		alert("デイトレ: 持ち越しの建玉を照会できません。口座を確認してください", strings.Join(unconfirmed, "\n"))
		digest.Anomaly("daytrade.carry_unconfirmed", fmt.Sprintf("%d 件の注文を照会できず持ち越しを判定できません", len(unconfirmed)))
	}
	if len(carried) == 0 {
		return nil, nil
	}
	lines := make([]string, 0, len(carried))
	for _, c := range carried {
		lines = append(lines, c.String())
	}
	fmt.Printf("持ち越し %d 件を成行で手仕舞います: %s\n", len(carried), strings.Join(lines, "、"))
	logWarn("daytrade.carry", "持ち越しを手仕舞う", map[string]any{"count": len(carried), "positions": lines, "phrase": phrase})
	digest.Anomaly("daytrade.carry", fmt.Sprintf("%d 件の持ち越しを手仕舞い", len(carried)))
	if failures := execute.ReturnCarried(env, b, carried, phrase); len(failures) > 0 {
		alert(fmt.Sprintf("デイトレ: %d 件の持ち越しの手仕舞いが通らず", len(failures)), strings.Join(failures, "\n"))
		digest.Anomaly("daytrade.carry_failed", fmt.Sprintf("%d 件の持ち越しの手仕舞いが通らず", len(failures)))
	}
	return carried, nil
}
