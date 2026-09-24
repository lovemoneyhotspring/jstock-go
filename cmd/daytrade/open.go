package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	dtevaluate "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/evaluate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/execute"
	dthistory "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/history"
	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/regime"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/selection"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
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

// openState は runOpen の 1 回の実行で、段（準備・plan・台帳と接続・気配・判定・選定・発注）を
// またいで持ち回る値。段ごとの関数はこれを読み書きする。
type openState struct {
	opts openOptions
	cfg  dtconfig.Config
	// now は開始時に 1 回だけ読んだ時刻。判定日・締め切り・時間帯・気配の鮮度はこれで見る
	now     time.Time
	day     time.Time
	started time.Time
	// deadline は時間帯の終わり（live のとき）と開始 + max_run_seconds の早い方
	deadline  time.Time
	watchOnly bool
	cal       *calendar.Calendar

	p dtplan.Plan
	// corpStale は材料の記録簿を使えずショートを見送る理由（空なら使えた）。
	// corpDropped は材料でショートの対象から外した銘柄
	corpStale   string
	corpDropped []string
}

func runOpen(opts openOptions) error {
	s, done, err := prepareOpen(opts)
	if done || err != nil {
		return err
	}

	if err := s.loadPlan(); err != nil {
		return err
	}

	allowed, reason := appSettings.CanExecuteLive(s.opts.live, s.cfg.Execution.KillSwitch)
	led, err := dtledger.Open(appSettings.DaytradeDBPath())
	if err != nil {
		return err
	}
	led.Verify = s.opts.brokerVerify
	defer led.Close()
	// 実行品質の記録は最後にまとめて書き出す（1 発注 1 ファイルにしない）
	defer flushExecution(s.day)
	// with-lock.sh の打ち切り（SIGTERM）では defer が走らない。固まった回の滑りが丸ごと
	// 消えるので、受け取った時点で貯めた分だけ書き出してから終える（Flush は mutex 付き）
	defer flushOnSignal(s.day)()

	env := execute.Env{
		Cfg: s.cfg, Ledger: led, Day: s.day, Report: run, Out: os.Stdout,
		RetryWait: execute.DefaultRetryWait, Deadline: s.deadline,
		// 寄る前の回か（9:00 より前）。真なら preopen_legs の脚を寄成で出す
		Preopen: s.cfg.Execution.PreopenAt(s.now, jst),
	}
	var b broker.Broker
	var carried []execute.Carried
	var held broker.LegPositions
	if allowed {
		if b, err = openBroker(s.cfg); err != nil {
			return err
		}
		broker.SetDeadline(b, s.deadline)
		// 前回の実行で送信結果が分からなかった注文があれば、ここで判定して台帳を直す。
		// 届いていなければ UNSENT になり、下の「発注済み」には数えない（種を変えて送り直す）
		if err := resolvePending(env, b); err != nil {
			return err
		}
		// 前営業日以前の建玉が残っていれば（引けで返済できなかった持ち越し）、新規に建てる前に
		// 寄付の成行で手仕舞う。検証は margin.carry_penalty で「翌寄りで返済」としているので同じにする。
		// 判定できなければ止める——持ち越しを知らずに建てると二重になりうる
		settled, err := execute.SettleCarried(env, b, execute.SettleAtOpen)
		noteSettlement(settled)
		if err != nil {
			return err
		}
		carried, held = settled.Carried, settled.Held
	} else {
		// dry-run は確認のたびに増える。その日の古い dry-run は消して最新だけ残す
		if _, err := led.ClearDryRun(s.day); err != nil {
			return err
		}
	}
	// 生きている／約定した建玉の数。再実行は「N − これ」だけを建てる——1 回目が途中で
	// 落ちても（通信エラー・締め切り）、次の cron が残りを埋める。拒否・失効は数えない
	placed, err := execute.PlacedToday(env)
	if err != nil {
		return err
	}
	remainingLong, remainingShort := execute.Remaining(s.cfg, placed, s.watchOnly)
	// 持ち越しが拘束している資金（残り株数 × 建値）。返済注文は出したが、寄っていない銘柄は
	// まだ約定しておらず資金は戻っていない。件数と予算への反映は、倍率を掛けた後の予算が
	// 決まったところで行う（execute.SizeDay）
	tiedLong, tiedShort := execute.TiedCapital(carried)
	if placed.Total() > 0 {
		// 余りをロングに回す設定では件数だけで「済み」と言わない（execute.DoneForToday）
		if execute.DoneForToday(s.cfg, placed, s.watchOnly) {
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

	eligible := s.p.Eligible()
	symbols := s.p.Symbols(eligible)
	shortUniverse := s.p.ShortEligible()
	if s.corpStale != "" || s.cfg.Margin.Paused {
		shortUniverse = nil
	}
	if s.cfg.Margin.Enabled && !s.watchOnly {
		// ショートの母集団はロングと別なので、気配はその和集合で取る
		symbols = mergeSymbols(symbols, s.p.Symbols(shortUniverse))
	}

	quotesStarted := clock.NowUTC()
	received, err := fetchQuotes(s.cfg, b, symbols, s.opts.quoteSource, s.opts.quoteFile, s.deadline)
	if err != nil {
		fmt.Println(err)
		logError("daytrade.skip", "気配が取れず寄付の買いを見送り", map[string]any{"reason": "no_quotes", "error": err.Error()})
		alert("デイトレ: 気配が取れず寄付の買いを見送り", fmt.Sprintf("%s 候補 %d 銘柄: %v", s.day.Format(DateLayout), len(symbols), err))
		digest.Anomaly("daytrade.no_quotes", err.Error())
		return nil
	}
	quotes, stale, delayed, bookKept := dtquotes.Fresh(received, s.cfg.Execution.MaxQuoteAge, s.now, s.opts.allowDelayed)
	// 気配の時刻（tDPP:T）には今日の日付を当てている。寄り前の銘柄で前日の時刻が返ると
	// 「今日の 15:30」＝未来として鮮度の検査を素通りする。実機で確かめるまで、除外した
	// 銘柄の時刻と年齢、未来の時刻を持つ銘柄の数を残す（docs/OPENING_DATA.md「実機で確かめること」）
	future := dtquotes.FutureStamped(received, s.now, futureSlack)
	// 気配の内訳は毎回残す（朝の点検 deploy/open-pipeline.jq が回ごとに並べる）。警告にするのは
	// 実際に除外した（stale / delayed）ときと未来の時刻があったときだけ——板で残しただけの回まで
	// 「使えない気配を除外」と書くと、除外 0 件でも警告に見える（2026-09-15 の朝がそうだった）
	breakdown := map[string]any{
		"received": len(received), "usable": len(quotes),
		"stale": len(stale), "stale_sample": dtquotes.DescribeAges(received, sample(stale), s.now),
		"delayed": len(delayed), "delayed_sample": sample(delayed),
		"future": len(future), "future_sample": dtquotes.DescribeAges(received, sample(future), s.now),
		// book_kept は現在値時刻が古くても板が返っていたので残した銘柄
		// （tDPP:T は最後の約定時刻なので、約定の薄い銘柄はここに入る）
		"book_kept": len(bookKept), "book_kept_sample": dtquotes.DescribeAges(received, sample(bookKept), s.now),
		"max_age_sec": s.cfg.Execution.MaxQuoteAge,
	}
	if len(stale) > 0 || len(delayed) > 0 || len(future) > 0 {
		logWarn("daytrade.quotes", "使えない気配を除外", breakdown)
	} else {
		logInfo("daytrade.quotes", "気配の内訳（除外なし）", breakdown)
	}

	prevAll := s.p.PrevCloseBySymbol()
	appendHistory(dthistory.KindQuotes, dthistory.QuotesFrame(received, quotes, prevAll), s.day)

	summary := map[string]any{
		"mode":             modeOf(s.watchOnly, allowed),
		"quotes_requested": len(symbols),
		"quotes_received":  len(received),
		"quotes_usable":    len(quotes),
		// 現在値時刻は古いが板が返っていたので残した銘柄（寄付が遅れる＝利益源）
		"quotes_book_kept": len(bookKept),
		// signal.skip_opened で外した「既に寄っていた」銘柄の数（設定が偽なら null）
		"quotes_opened": nil,
		// margin.spill_to_long でロングに回したショートの余り（円。回さなかった日は null）
		"spill":         nil,
		"already_long":  placed.Long,
		"already_short": placed.Short,
		"deadline":      deadlineText(s.deadline),
		"broker_verify": s.opts.brokerVerify,
		// 材料（TOB・MBO など）でショートの対象から外した銘柄と、記録簿が使えずショートを見送った理由
		"corp_excluded": strings.Join(s.corpDropped, ","),
		"news_stale":    s.corpStale,
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
		row["elapsed_ms"] = clock.NowUTC().Sub(s.started).Milliseconds()
		appendHistory(dthistory.KindOpenRun, dthistory.OpenRunFrame(row), s.day)
	}

	if len(quotes) == 0 {
		fmt.Println("使える気配がありません。発注しません")
		logError("daytrade.skip", "気配が無いため見送り", map[string]any{"reason": "no_quotes"})
		alert("デイトレ: 気配が取れず寄付の買いを見送り",
			fmt.Sprintf("%s 候補 %d 銘柄", s.day.Format(DateLayout), len(symbols)))
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
	// ロングの並べ方を寄付の判定より先に決める: LightGBM で並べられない日は gap_vol で取引し、
	// 米国小幅高の日は両脚とも休む（us_skip_legs を all に戻す。config.FallbackToGapVol）
	s.cfg = resolveRankBy(s.cfg, s.p, eligible, quotes)
	verdict, usStale, err := evaluateRegime(s.cfg, s.p, s.day, regime.MarketGapOf(gaps), led, env.Preopen, s.deadline)
	if err != nil {
		return err
	}
	// 並べ方は米国小幅高の日だけ替わる（signal.rank_by_us_low）。危険信号の判定より後でしか
	// 決まらないので、summary への記録もここまで待つ
	// 寄指の位置も米国小幅高の日だけ替わる（execution.preopen_limit_pct_us_low）。この設定の日のロングは
	// 寄る前の回の寄指でしか建てない——9:00 以降の回は見送りにする
	s.cfg = applyDayConfig(s.cfg, &env, verdict.UsLow)
	verdict = usLowPreopenOnly(s.cfg, verdict, env.Preopen)
	summary["rank_by"] = s.cfg.Signal.RankBy
	summary["us_low"] = verdict.UsLow
	summary["preopen_limit_pct"] = s.cfg.Execution.PreopenLimitPct
	summary["trade"] = verdict.Trade
	summary["reasons"] = strings.Join(verdict.Reasons, "、")
	summary["scale"] = verdict.Scale
	for k, v := range verdict.Notes {
		summary[k] = v
	}
	// 前夜の米国市場がまだ取れていない回は、待つ時刻（regime.us_stale_wait_until）までは判定せず
	// 次の回に任せる。前々夜の値で見送り・取引を決めない（見送りの順位表も積まない）
	if usStale {
		want := usmarket.ExpectedSession(s.day).Format(DateLayout)
		if s.cfg.Regime.WaitsForUs(clock.NowUTC(), jst) {
			fmt.Printf("前夜（%s）の米国市場がまだ取れていません。%s までは判定せず次の回を待ちます\n",
				want, s.cfg.Regime.UsStaleWaitUntil)
			logInfo("daytrade.skip", "前夜の米国市場を待って見送り",
				map[string]any{"reason": "us_stale", "want": want, "until": s.cfg.Regime.UsStaleWaitUntil})
			digest.Skipped("us_stale")
			finish("us_stale", map[string]any{"trade": false, "reasons": "前夜 " + want + " の米国市場を待つ"})
			return nil
		}
		logWarn("daytrade.us_stale", "前夜の米国市場が取れないまま判定", map[string]any{"want": want})
		digest.Anomaly("daytrade.us_stale", "前夜 "+want+" の米国市場が取れないまま判定")
	}
	if !verdict.Trade {
		fmt.Println("危険信号により今日は取引しません: " + strings.Join(verdict.Reasons, "、"))
		logInfo("daytrade.skip", "危険信号で見送り", map[string]any{"reason": "regime", "reasons": verdict.Reasons})
		digest.Note(map[string]any{"regime_skip": strings.Join(verdict.Reasons, "、")})
		// 見送りの日も「建てていたら」の順位表を残す。無いと evaluate が始値で作り直すので、
		// 9:01 の気配で何を選んでいたかが消え、dt_missed も欠けと見送りを見分けられない
		skippedQuotes, _ := execute.RankQuotes(quotes, placed.Symbols, execute.SweptSymbols(carried), s.cfg.Signal.SkipOpened)
		appendSkippedRanking(s.cfg, s.p, skippedQuotes, s.day)
		finish("regime", nil)
		return nil
	}

	// 件数と 1 注文の予算: 縮小 → ショック → 拘束 → ショートの倍率 → （選定の後に）余り。
	// その日の全体で決めてから、今日すでに建てた件数と金額を引く（execute.SizeDay）
	sizing := execute.SizeDay(execute.SizingInput{
		Cfg: s.cfg, Verdict: verdict, Placed: placed, TiedLong: tiedLong, TiedShort: tiedShort,
		WatchOnly: s.watchOnly, WatchRows: watchRows,
	})
	execute.EmitNotes(env, sizing.Notes)
	weak := sizing.Weak
	weighting := sizing.Long.Weighting

	// 候補の気配: 今日建てた銘柄・台帳外として返済に回した銘柄を落とし、signal.skip_opened なら
	// 9:01 の時点で既に寄っている銘柄も落とす。順位付けの直前に気配そのものを落とすので、
	// ロング・ショートの両方に効く（市場ギャップと危険信号は落とす前の気配で見る——候補全体の
	// 分布が変わるため）
	swept := execute.SweptSymbols(carried)
	if len(swept) > 0 {
		logWarn("daytrade.sweep", "台帳外の返済に回した銘柄を今日の候補から外す",
			map[string]any{"symbols": sortedKeys(swept)})
	}
	rankQuotes, dropped := execute.RankQuotes(quotes, placed.Symbols, swept, s.cfg.Signal.SkipOpened)
	if s.cfg.Signal.SkipOpened {
		summary["quotes_opened"] = int64(len(dropped))
		fmt.Printf("既に寄っている %d 銘柄を候補から外しました（signal.skip_opened。残り %d）\n",
			len(dropped), len(rankQuotes))
		logInfo("daytrade.quotes", "既に寄っている銘柄を除外", map[string]any{
			"opened": len(dropped), "opened_sample": sample(dropped), "remaining": len(rankQuotes),
		})
	}

	// ショートの脚（[margin]）を**先に**決める: 使わなかった資金をロングに回すため
	// （margin.spill_to_long。検証の simulateMarginSpill と同じ順序）。資金はシーソー
	shortMultiplier := sizing.ShortMultiplier
	var (
		shortRanking []selection.Ranked
		shortPicks   []selection.Pick
		shortReasons map[string]string
		shortN       int
		shortBudget  decimal.Decimal
	)
	if sizing.ShortOpen {
		shortN, shortBudget = sizing.Short.N, sizing.Short.Budget
		shortRanking = selection.RankShort(shortUniverse, rankQuotes, s.cfg.Margin)
		shortOpts := selection.PickOptions{
			N: shortN, Budget: shortBudget, Weighting: sizing.Short.Weighting, Side: domain.SideSell,
			MaxAmount: s.cfg.Margin.MaxOrder,
		}
		shortPicks = selection.PickFrom(shortRanking, shortOpts)
		shortReasons = selection.PickReasons(shortRanking, shortOpts, shortPicks)
	}

	// ショートの余り（候補が無い・上限で頭打ち）をロングに回す。銘柄数は総予算 ÷ 1 注文の
	// 予算（capital.max_positions が上限）。倍率 0 の日（ショック日）は回す元が無い。
	// 余りは今日のショートの総予算から、今日建てた分と今回の選定を引いたもの（再実行で数え直さない）
	long, spill, spillNotes := sizing.WithSpill(shortPicks)
	if s.corpStale != "" {
		// 記録簿が使えずショートを見送った回は余りを回さない。回したまま後の回で記録簿が読めると、
		// ショートは建てた金額 0 として満額で建ち、ロングに回した分と合わせて資金を超える
		long, spill, spillNotes = sizing.Long, decimal.Zero, nil
	}
	n, budget := long.N, long.Budget

	// 並べるのは**落とす前の**気配で。建て済みを落としてから採点すると、機械学習の特徴量
	// （候補の中での百分位）が 1 回目と変わる。落とすのは順位を付けた後（selection.Keep）
	ranking, err := selection.TryRank(eligible, quotes, s.cfg.Signal)
	if err != nil {
		// 試し並べ（resolveRankBy）は通ったのに、候補を絞った後で失敗した。寄付の判定は済んでいるので
		// gap_vol で並べて続ける。米国小幅高で「ショートだけ休む」と判定した日は、gap_vol なら
		// ロングも休む日なので建てない（順位表は残す）
		logWarn("daytrade.rerank", "LightGBM で並べられないため gap_vol で並べる（寄付の判定の後）",
			map[string]any{"error": err.Error(), "short_off": verdict.ShortOff})
		digest.Anomaly("daytrade.rerank", "LightGBM で並べられず gap_vol で取引（判定の後）: "+err.Error())
		fmt.Printf("LightGBM で並べられないため gap_vol で並べます: %v\n", err)
		s.cfg = s.cfg.FallbackToGapVol()
		summary["rank_by"] = s.cfg.Signal.RankBy
		ranking = selection.Rank(eligible, quotes, s.cfg.Signal)
		if verdict.ShortOff {
			// gap_vol はこの日を両脚とも休む（us_skip_legs = "all"）。危険信号で見送った日と
			// 同じ形で終える——余りをロングに回した通知だけ出して no_picks で終わると、
			// 見送りの印の無い順位表が残り、評価が「候補なし」と読む
			fmt.Printf("gap_vol の米国小幅高の日なのでロングも休みます（%s）\n", verdict.ShortOffReason)
			logInfo("daytrade.skip", "gap_vol に戻して見送り",
				map[string]any{"reason": "regime", "reasons": verdict.ShortOffReason})
			digest.Note(map[string]any{"regime_skip": verdict.ShortOffReason})
			appendSkippedRanking(s.cfg, s.p, rankQuotes, s.day)
			finish("regime", map[string]any{"trade": false, "reasons": verdict.ShortOffReason})
			return nil
		}
	}
	// 余りをロングに回す通知は、並べ替えの失敗で見送る日を除いてから出す
	if spill.IsPositive() {
		execute.EmitNotes(env, spillNotes)
		summary["spill"] = spill
	}
	// 今日すでに建てた銘柄・返済に回した銘柄（と signal.skip_opened なら寄った銘柄）を、
	// 順位を付けた後に落とす
	ranking = selection.Keep(ranking, rankQuotes)
	// 業種の上限を掛ける設定なのに業種が取れていないと、判定は黙って素通りする
	// （2026-09-12 に発覚：plan の parquet に sector 列が無く、本番だけ無制限だった）。
	// 古い plan を読んだときも気付けるように、ここで鳴らす。
	if s.cfg.Signal.MaxPerSector > 0 {
		noSector := 0
		for _, r := range ranking {
			if r.Sector == "" {
				noSector++
			}
		}
		if noSector > 0 {
			logWarn("daytrade.sector", "業種が取れない候補があるため max_per_sector が効かない",
				map[string]any{"no_sector": noSector, "ranked": len(ranking), "max_per_sector": s.cfg.Signal.MaxPerSector})
			fmt.Printf("業種の取れない候補 %d/%d 件——max_per_sector=%d はその分効きません\n",
				noSector, len(ranking), s.cfg.Signal.MaxPerSector)
		}
	}
	longOpts := selection.PickOptions{
		N: n, Budget: budget, Weighting: weighting, Side: domain.SideBuy,
		MaxAmount: s.cfg.Capital.MaxOrder, ValuePool: s.cfg.Signal.ValuePool,
		MaxPerSector: s.cfg.Signal.MaxPerSector,
	}
	picks := selection.PickFrom(ranking, longOpts)
	longReasons := selection.PickReasons(ranking, longOpts, picks)
	longPicks := len(picks)
	rulePicks := selection.RulePicks(ranking, longOpts, picks)
	// 既存規則（gap_vol）は米国小幅高の日を両脚とも休む（us_skip_legs = "all"）。LightGBM だけが
	// 取引するこの日に同じ N で選んだことにすると、gap_vol が建てない日の成績が比べに混ざるので
	// 比べる相手を 0 件にして、候補なしと区別する印（rule_off）を残す
	ruleOff := verdict.ShortOff && s.cfg.Signal.RankBy == dtconfig.RankByLGBM
	if ruleOff {
		rulePicks = nil
	}
	summary["rule_off"] = ruleOff
	longFrame := dthistory.RankingFrame(ranking, picks, rulePicks, "BUY", n, budget, longReasons)
	if ruleOff {
		longFrame = dthistory.MarkRuleOff(longFrame)
	}
	frames := []history.Frame{longFrame}
	summary["n"], summary["budget"], summary["weighting"], summary["weak"] = n, budget, weighting, weak
	printPicks(picks, len(rankQuotes), s.p, s.watchOnly, "")
	if s.cfg.Signal.RankBy == dtconfig.RankByLGBM && len(ranking) > 0 && ranking[0].Score != nil {
		// 既存規則は参考として並べて出す（発注はしない。順位表の rule_picked にも残る）
		names := make([]string, 0, len(rulePicks))
		for _, rp := range rulePicks {
			names = append(names, rp.Symbol)
		}
		fmt.Printf("  参考: 既存規則（gap_vol）なら %s\n", strings.Join(names, " "))
	}
	logRanking(s.day, "BUY", ranking, picks, longReasons, n, budget, verdict.Scale, weighting, len(rankQuotes))

	switch {
	case s.cfg.Margin.Enabled && !s.watchOnly && s.cfg.Margin.Paused:
		// 一時停止中は倍率を 1 のまま残して枠をロングへ回す（execute.SizeDay）。ショートの候補も
		// 取らないので SELL の順位表は積まない——0 件の SELL を「候補なし」と読ませないため。
		// 倍率 0 のショック日は回す枠が無いので、そのことも書き分ける
		if spill.IsPositive() {
			fmt.Printf("ショート: 一時停止中（margin.paused）。枠 %s 円はロングに回しました\n", yen(spill))
		} else {
			fmt.Println("ショート: 一時停止中（margin.paused）。この回にロングへ回す枠はありません")
		}
		summary["short_paused"] = true
	case shortMultiplier.GreaterThan(decimal.Zero):
		label := "通常日"
		if weak {
			label = "弱い日"
		}
		fmt.Printf("ショート: %sの倍率 %s × 1 注文 %s 円 = %s 円  対象 %d 銘柄\n",
			label, shortMultiplier.String(), yen(s.cfg.Margin.BudgetPerOrder()), yen(shortBudget), len(shortUniverse))
		printPicks(shortPicks, len(rankQuotes), s.p, false, "寄付の売建（信用）")
		frames = append(frames, dthistory.RankingFrame(shortRanking, shortPicks, shortPicks, "SELL", shortN, shortBudget, shortReasons))
		summary["short_n"] = shortN
		summary["short_budget"] = shortBudget
		summary["short_multiplier"] = shortMultiplier
		logRanking(s.day, "SELL", shortRanking, shortPicks, shortReasons, shortN, shortBudget, verdict.Scale, s.cfg.Margin.Weighting, len(rankQuotes))
		picks = append(picks, shortPicks...)
	case s.cfg.Margin.Enabled && !s.watchOnly && remainingShort <= 0 && placed.Short > 0:
		fmt.Printf("ショート: 発注済み（%d 件）\n", placed.Short)
	case s.cfg.Margin.Enabled && !s.watchOnly:
		fmt.Println("ショート: この日は建てない（倍率 0）")
	}
	if path := appendHistory(dthistory.KindRanking, concatFrames(frames), s.day); path != "" {
		fmt.Printf("履歴に追記 %s\n", path)
	}
	summary["long_picks"] = longPicks
	summary["short_picks"] = len(picks) - longPicks

	if len(picks) == 0 {
		logInfo("daytrade.skip", "条件に合う銘柄なし", map[string]any{"reason": "no_picks", "quotes": len(quotes)})
		finish("no_picks", nil)
		return nil
	}
	if s.watchOnly {
		logInfo("daytrade.skip", "資金 0 のため買わない", map[string]any{"reason": "no_capital", "picks": len(picks)})
		finish("no_capital", nil)
		return nil
	}
	if err := confirmLive(allowed, s.opts.yes); err != nil {
		return err
	}

	if allowed {
		// 台帳に無い建玉がブローカーにあれば、この実行は二重に建てることになる。
		// 冪等性は台帳の client_order_id で担保しているので、台帳を失う・別ホストへ
		// 移す・復元した直後は効かない。発注の直前にブローカーと突き合わせる
		if err := execute.EnsureNoUnrecordedPositions(env, held, picks, carried); err != nil {
			var unrecorded *execute.ErrUnrecordedPositions
			if errors.As(err, &unrecorded) {
				digest.Anomaly("daytrade.unrecorded_positions",
					fmt.Sprintf("%d 銘柄に台帳外の建玉", len(unrecorded.Positions)))
			}
			return err
		}
	}

	// 米国小幅高の日を寄指だけで取引する設定では、指値を作れない銘柄（前日終値が無い・呼値に丸められない）を
	// 寄成で出さない。平常日は寄成に戻るだけでよいが、この日の寄成は損の側（−9〜−12 bp/日）
	if verdict.UsLow && s.cfg.Execution.UsLowPreopenOnly() {
		before := len(picks)
		picks = dropWithoutOpeningLimit(picks, s.cfg)
		summary["opening_limit_dropped"] = before - len(picks)
	}

	// 台帳に残す「送る直前の時価」は、取ったばかりの気配があればそれを使う（取り直すと順位表と
	// 1 本目の注文の間に往復が 1 つ挟まる）。年齢は**取り始め**から測る——120 銘柄ずつの直列なので
	// 先頭のバッチがいちばん古い。古ければ渡さず、PlacePicks が従来どおり取り直す
	if age := clock.NowUTC().Sub(quotesStarted); allowed && age <= refReuseMaxAge {
		env.RefPrices = execute.RefPricesFromQuotes(received, picks)
		logInfo("daytrade.ref_price", "執行時の時価に選定の気配を使う", map[string]any{
			"age_ms": age.Milliseconds(), "reused": env.RefPrices != nil, "picks": len(picks)})
	}
	orders, failures, err := execute.PlacePicks(env, b, picks)
	run.FlushAlerts()
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
		"elapsed_ms": clock.NowUTC().Sub(s.started).Milliseconds(), "deadline": deadlineText(s.deadline),
	})
	digest.Note(map[string]any{
		"phase": "open", "live": allowed, "picks": len(picks), "failures": len(failures),
	})
	return nil
}

// loadPlan は判定日の plan を読み（無ければ作り）、材料（TOB・MBO など）の印を付け直して表示する。
// 記録簿を使えない朝は corpStale に理由を入れる（ショートを見送る。ロングは止めない）。
func (s *openState) loadPlan() error {
	p, ok, err := dtplan.Load(appSettings.DaytradeDir(), s.day)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("%s の候補が無いので今作ります（前夜の plan が走っていません）\n", s.day.Format(DateLayout))
		if p, err = buildPlan(s.cfg, s.day); err != nil {
			return err
		}
	}
	p = refreshIV(s.cfg, p)
	// 材料（TOB・MBO など）の印をこの時点の記録簿で付け直す。前夜の plan より後の公表
	// （20:30 以降・朝の開示）を拾うため。記録簿が読めない・古いときは、その朝の公表を知らないまま
	// 売らないようにショートを見送る（ロングは止めない）
	corpStale := ""
	var corpDropped []string
	// ショートの一時停止中（margin.paused）は売らないので、記録簿の鮮度でショートを見送る判定も要らない
	if s.cfg.Margin.Enabled && s.cfg.Margin.ExcludeCorpEvents && !s.cfg.Margin.Paused {
		ev, dropped, err := markPlanCorpEvents(s.cfg, &p, s.day, s.now, s.cal.Closed)
		if err != nil {
			corpStale = err.Error()
		} else {
			corpStale = ev.staleness(s.now, s.cfg.Margin.CorpEventMaxStalenessMinutes)
		}
		corpDropped = dropped
		if corpStale != "" {
			fmt.Println("ニュースの記録簿を使えないため、ショートを見送ります: " + corpStale)
			logWarn("daytrade.news_stale", "ニュースの記録簿を使えずショートを見送り", map[string]any{"reason": corpStale})
			digest.Anomaly("daytrade.news_stale", "ショートを見送り: "+corpStale)
		}
	}
	printPlan(p, s.cfg)
	s.p, s.corpStale, s.corpDropped = p, corpStale, corpDropped
	return nil
}

// prepareOpen は設定を読み、判定日と締め切りを決める。戦略が無効・休場日・半日立会・
// 時間帯の外なら、見送りを記録して done を返す（err は nil）。
func prepareOpen(opts openOptions) (s *openState, done bool, err error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, false, err
	}
	// 以降のログの全行とダイジェストに印を付ける（env とは独立）
	run.SetVerify(opts.brokerVerify)
	if !cfg.Capital.Enabled {
		fmt.Println("jp_gap_fade は無効（capital.enabled = false）。何もしません")
		logInfo("daytrade.skip", "戦略が無効", map[string]any{"reason": "disabled"})
		digest.Skipped("disabled")
		return nil, true, nil
	}
	fmt.Println(appSettings.DescribeMode(opts.live, cfg.Execution.KillSwitch))

	now := clock.NowUTC()
	day, err := dayOrToday(opts.date, now)
	if err != nil {
		return nil, false, err
	}
	// 保証金で建玉の上限を安全側へ寄せる（下げ方向のみ）。朝 8:53 の warm-margin が焼いた
	// キャッシュを読むだけなので、ここでブローカーには繋がない。取れない朝は設定の値のまま建てる。
	// **watchOnly の判定より前**でなければ、下げた結果が今日の判断に効かない
	// 運用通知は同期の HTTP。発注の前に挟むと Discord が遅い日に注文が遅れるので、
	// 注文を出し切ってから送る（途中で抜けた回は run.Finish が送る）
	if opts.live {
		run.DeferAlerts()
	}
	cfg = applyMarginCap(cfg, day)
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
		"preopen": cfg.Execution.PreopenAt(now, jst), "preopen_legs": cfg.Execution.PreopenLegs,
		// 寄指の位置は設定の値（その日に使った値は open_run の preopen_limit_pct）
		"preopen_limit_pct":        cfg.Execution.PreopenLimitPct.String(),
		"preopen_limit_pct_us_low": cfg.Execution.PreopenLimitPctUsLow.String(),
		"rank_by_us_low":           cfg.Signal.RankByUsLow, "us_skip_legs": cfg.Regime.UsSkipLegs,
		"max_run_seconds": cfg.Execution.MaxRunSeconds,
		"broker_verify":   opts.brokerVerify,
	})
	if watchOnly {
		fmt.Println("資金 0（max_capital = 0）: スクリーニングと候補の表示だけ行い、買いません")
	}
	cal, holiday := holidayCalendar(day, "open", opts.live)
	if holiday || skipHalfDay(cal, cfg, day, "open") {
		return nil, true, nil
	}
	if opts.live && !opts.ignoreWindow && !cfg.Execution.InWindow("entry", now, jst) {
		fmt.Printf("発注時間帯の外（%s）。何もしません\n", describeWindow(cfg, "entry"))
		logInfo("daytrade.skip", "発注時間帯の外",
			map[string]any{"reason": "window", "window": describeWindow(cfg, "entry")})
		digest.Skipped("window")
		return nil, true, nil
	}
	return &openState{
		opts: opts, cfg: cfg, now: now, day: day, started: started,
		deadline: deadline, watchOnly: watchOnly, cal: cal,
	}, false, nil
}

// resolveRankBy はその日のロングの並べ方を決める。rank_by = "lgbm" でも、plan が古い・モデルが
// 読めない・当日の気配で試しに並べて失敗した、のどれかなら gap_vol の設定（FallbackToGapVol）を返す。
// 取引は止めない。戻した日は異常として残す（日次レポートと night-repair が拾う）。
func resolveRankBy(cfg dtconfig.Config, p dtplan.Plan, eligible []universe.Candidate, quotes map[string]selection.Quote) dtconfig.Config {
	resolved, reason := p.RankConfig(cfg)
	// 米国小幅高の日だけ LightGBM を使う設定（rank_by_us_low）でも、当日の気配で試し並べしておく。
	// 小幅高の日の朝になって初めて失敗するより、毎朝 1 回試して落ちる日を先に見つけるほうがよい
	if reason == "" && resolved.Signal.UsesLGBM() {
		if _, err := selection.TryRank(eligible, quotes, resolved.Signal.ForDay(true)); err != nil {
			resolved, reason = cfg.FallbackToGapVol(), err.Error()
		}
	}
	if reason == "" {
		return resolved
	}
	logWarn("daytrade.rerank", "LightGBM で並べられないため gap_vol で取引する", map[string]any{
		"rank_by": cfg.Signal.RankBy, "fallback": resolved.Signal.RankBy, "reason": reason,
		"plan_features": p.Meta.RerankFeatures, "us_skip_legs": resolved.Regime.UsSkipLegs,
	})
	digest.Anomaly("daytrade.rerank", "LightGBM で並べられず gap_vol で取引: "+reason)
	fmt.Printf("LightGBM で並べられないため gap_vol で並べます（米国小幅高の日は両脚とも休む）: %s\n", reason)
	return resolved
}

// evaluateRegime は危険信号を評価し、ログに残す。usStale は米国の信号を使う設定で、前夜の
// セッション（usmarket.ExpectedSession）がまだ取れていないか（取得元の公開遅れ・障害）。
func evaluateRegime(cfg dtconfig.Config, p dtplan.Plan, day time.Time, marketGap *float64, led *dtledger.Ledger, preopen bool, deadline time.Time) (regime.Verdict, bool, error) {
	signals := regime.Signals{
		Day:       day,
		IVPrev:    p.Meta.IVPrev,
		Drift:     p.Meta.Drift,
		MarketGap: marketGap,
	}
	if cfg.Regime.EquityCurveDays > 0 {
		recent, err := recentPnL(cfg, day, led)
		if err != nil {
			return regime.Verdict{}, false, err
		}
		signals.RecentPnL = recent
	}
	usStale := false
	if usmarketNeeded(cfg) {
		// 前夜の plan が温めたキャッシュを先に見る。取りに行くときも 1 本 8 秒まで——
		// 寄付の判断に FRED の遅さを持ち込まない（取れなければゲートは効かせない）
		// 朝の warm-us が焼いていればキャッシュを読むだけ。焼けていない朝だけ取りに行く。
		// 寄る前の回は 9:00:00 までに寄成を届けなければ意味が無いので、待つ上限を詰める
		// その上限も締め切り（板寄せ）から逆算して頭打ちにする（usFetchLimits）
		timeout, budget := usFetchLimits(preopen, deadline, clock.NowUTC())
		session, source, err := usmarketLatest(day, timeout, budget)
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
			} else {
				// S&P500 は取れているので us_missing には出ない。黙って落ちると
				// 気付けないので必ず残す（2026-09-16 まで無音だった）
				logWarn("daytrade.us_vix_missing", "VIX が取れていない（米国のゲートの例外が効かない）",
					map[string]any{"session": session.Describe(), "source": source})
			}
		}
		// FRED だけだった頃は、火〜金の 9:01〜9:10 が前々夜の値で判定していた（前夜の値が出るのは
		// 9:10 JST ごろ。2026-09-15 は 9/11 の値で 4 回見送り、9:13 に 9/14 の値で取引した）
		usStale = !usmarket.IsFresh(session, day)
	}
	verdict := regime.Evaluate(cfg.Regime, signals)
	fields := map[string]any{"day": day.Format(DateLayout), "trade": verdict.Trade, "reasons": verdict.Reasons, "us_stale": usStale}
	for k, v := range verdict.Notes {
		fields[k] = v
	}
	logInfo("daytrade.regime", "危険信号", fields)
	return verdict, usStale, nil
}

// applyDayConfig はその日（米国小幅高かどうか）の並べ方と寄指の位置を設定に入れる。
// **発注（execute.EntryRequest）が読むのは env.Cfg の側**なので、そちらにも入れる——抜けると
// 小幅高の日のロングが黙って寄成で出る。
func applyDayConfig(cfg dtconfig.Config, env *execute.Env, usLow bool) dtconfig.Config {
	cfg.Signal = cfg.Signal.ForDay(usLow)
	cfg.Execution = cfg.Execution.ForDay(usLow)
	env.Cfg.Execution = cfg.Execution
	return cfg
}

// usLowPreopenOnly は米国小幅高の日を「寄る前の回の寄指だけ」で取引する設定のとき、9:00 以降の回を
// 見送りにする（execution.preopen_limit_pct_us_low）。この日のロングを成行で建てると、どの並べ方でも
// −9〜−12 bp/日——寄る前の回が走らなかった・米国の値が寄る前に取れなかった朝に、後の回が成行で埋めてはいけない。
// 前夜の米国の値が 9:12 を過ぎても取れない朝は小幅高かどうかが分からず、ここには掛からない
// （従来どおり平常日として取引する。us_stale の異常に載る）。
// 寄る前の回で約定しなかった寄指の枠は PlacedToday が使った枠に数えるが、それに頼らずここで止める。
func usLowPreopenOnly(cfg dtconfig.Config, verdict regime.Verdict, preopen bool) regime.Verdict {
	if !verdict.Trade || !verdict.UsLow || preopen || !cfg.Execution.UsLowPreopenOnly() {
		return verdict
	}
	verdict.Trade = false
	verdict.Reasons = append(verdict.Reasons, verdict.ShortOffReason+"、ロングは寄る前の寄指だけ（9:00 以降の回は建てない）")
	return verdict
}

// dropWithoutOpeningLimit は寄指の指値を作れないロングを発注から外す（米国小幅高の日を寄指だけで取引する設定）。
// execute.EntryRequest は指値を作れないと寄成で出すので、その前に落とす。ショートはこの日は選ばれていない。
func dropWithoutOpeningLimit(picks []selection.Pick, cfg dtconfig.Config) []selection.Pick {
	kept := make([]selection.Pick, 0, len(picks))
	for _, pick := range picks {
		if _, ok := execute.OpeningLimitPrice(pick, cfg); !ok {
			logWarn("daytrade.opening_limit", "寄指の指値を作れないため発注しない（米国小幅高の日は寄成で出さない）",
				map[string]any{"symbol": pick.Symbol, "side": string(pick.Side), "prev_close": pick.PrevClose.String()})
			fmt.Printf("  %s は寄指の指値を作れないため発注しません\n", pick.Symbol)
			continue
		}
		kept = append(kept, pick)
	}
	return kept
}

// appendSkippedRanking は危険信号で見送った日の順位表を、通常日の件数と予算で作って積む
// （skipped の印付き）。evaluate が気配の順位表として評価し、review が通常日と分けて集計する。
func appendSkippedRanking(cfg dtconfig.Config, p dtplan.Plan, quotes map[string]selection.Quote, day time.Time) {
	var frames []history.Frame
	for _, leg := range dtevaluate.NominalLegs(p, quotes, cfg) {
		frames = append(frames, dthistory.MarkSkipped(
			dthistory.RankingFrame(leg.Ranking, leg.Picks, leg.RulePicks, leg.Side, leg.N, leg.Budget, leg.Reasons)))
	}
	if path := appendHistory(dthistory.KindRanking, concatFrames(frames), day); path != "" {
		fmt.Printf("見送りの日の順位表（建てていたら）を履歴に追記 %s\n", path)
	}
}

// flushExecution は貯めた実行品質（滑り）の記録を書き出す。
func flushExecution(day time.Time) {
	if err := execution.Flush(historyStore(), day); err != nil {
		logWarn("daytrade.execution", "実行品質の記録に失敗", map[string]any{"error": err.Error()})
	}
}

// flushOnSignal は打ち切り（SIGTERM / Ctrl-C）を受けたら、貯めた実行品質の記録・保留した通知・
// ダイジェストを書き出して終える。with-lock.sh の打ち切りでは defer が走らないので、何もしないと
// 固まった回の記録が丸ごと消える。返り値を defer で呼んで後始末する。売買そのものは止めない
// ——記録は付帯物で、送信済みの注文は台帳の PENDING から次の回が判定する。
// 人への通知は with-lock.sh の側（WITH_LOCK_NOTIFY）。SIGKILL まで 10 秒しか無い。
func flushOnSignal(day time.Time) (stop func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-ch:
			logWarn("daytrade.signal", "打ち切りを受けたので記録を書き出して終了",
				map[string]any{"signal": sig.String()})
			flushExecution(day)
			run.Finish(fmt.Errorf("打ち切り（%s）", sig))
			os.Exit(143)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
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

// logRanking は順位表の上位をログに残す。行は N + rankingExtra 件か、最後に選ばれた順位までの
// 深い方——上位が予算超え・業種の上限で飛ばされると、選ばれた銘柄が N + 5 位より下に来る
// （2026-09-15 の 9:13 は 11 位と 13 位を建てた）。reason は selection.PickReasons。
func logRanking(day time.Time, side string, ranking []selection.Ranked, picks []selection.Pick, reasons map[string]string, n int, budget decimal.Decimal, scale float64, weighting string, quotes int) {
	picked := map[string]selection.Pick{}
	for _, p := range picks {
		picked[p.Symbol] = p
	}
	limit := min(len(ranking), n+rankingExtra)
	for i, r := range ranking {
		if _, ok := picked[r.Symbol]; ok && i+1 > limit {
			limit = i + 1
		}
	}
	rows := make([]map[string]any, 0, limit)
	for _, r := range ranking[:limit] {
		row := map[string]any{
			"rank": r.Rank, "symbol": r.Symbol, "name": r.Name, "gap": r.Gap.String(), "price": r.Price.String(),
			"picked": false, "reason": reasons[r.Symbol],
		}
		if r.Vol != nil {
			row["vol"] = *r.Vol
		}
		if r.Score != nil {
			row["score"], row["rule_rank"] = *r.Score, r.RuleRank
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

// 寄る前の回（8:59:45〜9:00:00）が米国市場を待つ上限。1 リクエスト 3 秒、合計 5 秒を過ぎたら
// 残りの取得元には繋がない（最悪でも 5 + 3 = 8 秒。取得元 3 つ × 系列 2 本を 8 秒ずつ待つと
// 最悪 48 秒で、窓の 14 秒を取得だけで使い切る）。取れなければ us_stale で見送り、
// 9:00:03 からの回が従来どおり取りに行く。実測は取れる朝で 1〜2 秒（2026-09-15〜17）。
const (
	usFetchTimeoutPreopen = 3 * time.Second
	usFetchBudgetPreopen  = 5 * time.Second
)

// usFetchReservePreopen は寄る前の回が、米国市場の取得の後に残しておく時間（並べる 0.3 秒 + 順位表の書き出しと
// 台帳外の建玉の検査で 0.5〜1 秒 + 注文 4 本の往復 1.2 秒 + execute.EntrySendMargin の 1 秒）。取得がここへ食い込むと、
// 取れても寄成が板寄せに間に合わない。米国小幅高の日は送れなかった分を後の回が埋めないので、詰めすぎない。
const usFetchReservePreopen = 4 * time.Second

// usFetchLimits は米国市場の取得の上限（1 リクエストの timeout と、次の取得元へ繋ぎ始めてよい budget）。
// 最悪の所要は budget + timeout。寄る前の回は、それが「締め切り − usFetchReservePreopen」を超えないように詰める
// ——気配が遅れた朝に 8 秒待つと、取れた頃には 9:00:00 を過ぎている（2026-09-21 のレビュー #9）。
// 余裕が無ければ実質キャッシュだけを見て、無ければ us_stale で見送る（9:00 以降の回が取りに行く）。
// budget = 0 は「上限なし」の意味なので、詰めた結果の 0 は 1 ms にする。
func usFetchLimits(preopen bool, deadline, now time.Time) (timeout, budget time.Duration) {
	if !preopen {
		return usFetchTimeout, 0
	}
	timeout, budget = usFetchTimeoutPreopen, usFetchBudgetPreopen
	if deadline.IsZero() {
		return timeout, budget
	}
	room := deadline.Sub(now) - usFetchReservePreopen
	if room >= timeout+budget {
		return timeout, budget
	}
	if room < time.Millisecond {
		room = time.Millisecond
	}
	if timeout > room {
		timeout = room
	}
	if budget = room - timeout; budget < time.Millisecond {
		budget = time.Millisecond
	}
	return timeout, budget
}

// refReuseMaxAge は選定の気配を「送る直前の時価」として使い回してよい年齢の上限（取り始めから）。
// 普段は気配 1.5〜1.9 秒 + 判定 0.3 秒で約 2 秒。取り直しても 4 本目の注文を送る頃には
// 1.5 秒ほど経っているので、3 秒までなら記録の意味は変わらない。判定が長引いた回
// （米国市場を取りに行った・材料の付け直しが重い）は超えるので、従来どおり取り直す。
const refReuseMaxAge = 3 * time.Second

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
// fetchQuotes は候補の気配を取る。b が立花の接続なら時価問合もそれで送る（接続と
// セッションの取り回しを増やさない）。nil（dry-run）なら取得元が自分で繋ぐ。
func fetchQuotes(cfg dtconfig.Config, b broker.Broker, symbols []string, sourceOverride, fileOverride string, deadline time.Time) (map[string]selection.Quote, error) {
	name := cfg.Execution.QuoteSource
	if sourceOverride != "" {
		name = sourceOverride
	}
	file := cfg.Execution.QuoteFile
	if fileOverride != "" {
		file = fileOverride
	}
	tachibana, _ := b.(*broker.TachibanaBroker)
	source, err := dtquotes.New(name, dtquotes.Params{
		Env: appSettings.Env, Dotenv: appSettings.DotenvMap,
		StateDir: appSettings.StateDir, QuoteFile: file,
		Logger: run, Deadline: deadline, Broker: tachibana,
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
	// 値段を板（最良気配）から取った銘柄の数。**まだ寄っていない銘柄**で、始値も現在値も
	// 空だったもの——利益源はここに集まる（docs/OPENING_DATA.md「実機で確かめること」3）。
	fromBook := 0
	for _, q := range found {
		if q.FromBook {
			fromBook++
		}
	}
	logInfo("daytrade.quotes", "気配を取得", map[string]any{
		"source": name, "requested": len(symbols), "received": len(found),
		"missing": len(missing), "missing_sample": sample(missing), "elapsed_ms": elapsed,
		"from_book": fromBook,
	})
	return found, nil
}

// noteSettlement は持ち越しの片付け（execute.SettleCarried。open / close 共用）の結果を
// ダイジェストの異常に残す。判定・返済・通知・ログは execute が行う。
func noteSettlement(s execute.Settlement) {
	if len(s.Unconfirmed) > 0 {
		digest.Anomaly("daytrade.carry_unconfirmed", fmt.Sprintf("%d 件の注文を照会できず持ち越しを判定できません", len(s.Unconfirmed)))
	}
	if len(s.Unrecorded) > 0 {
		digest.Anomaly("daytrade.sweep", fmt.Sprintf("台帳に無い信用建玉 %d 件を返済", len(s.Unrecorded)))
	}
	if len(s.Carried) > 0 {
		digest.Anomaly("daytrade.carry", fmt.Sprintf("%d 件の持ち越しを手仕舞い", len(s.Carried)))
	}
	if len(s.Failures) > 0 {
		digest.Anomaly("daytrade.carry_failed", fmt.Sprintf("%d 件の持ち越しの手仕舞いが通らず", len(s.Failures)))
	}
	if s.CheckErr != nil {
		digest.Anomaly("daytrade.carry_check_failed", "持ち越しを判定できず当日の手仕舞いだけ行った: "+s.CheckErr.Error())
	}
}
