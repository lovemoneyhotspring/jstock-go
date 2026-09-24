package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/calendar"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/engine"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/portfolio"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var liveFlag bool
	var yesFlag bool
	var noSyncFlag bool
	var brokerVerifyFlag bool
	var acceptFlatFlag bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "日次実行サイクルを実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.Crash("日次実行", "wbjp.crash",
				runDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag, acceptFlatFlag))
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	cmd.Flags().BoolVar(&noSyncFlag, "no-sync", false, "足の更新をしない")
	cmd.Flags().BoolVar(&brokerVerifyFlag, "broker-verify", false,
		"発注経路の実機検証（docs/BROKER_VERIFY.md）。ログとダイジェストに印を付ける")
	cmd.Flags().BoolVar(&acceptFlatFlag, "accept-flat", false,
		"建玉の照会が 0 件でも台帳の保有を捨てて続ける（口座が本当に空だと確かめたときだけ。1 回で失効。cron の行には書かない）")
	return cmd
}

// runDaily は本体。RunE から切り出してあるのは、異常終了を run.Crash で記録・通知するため。
func runDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag, acceptFlatFlag bool) (err error) {
	d, err := prepareDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag, acceptFlatFlag)
	if err != nil {
		return err
	}
	setCfg, stratCfg, canLive, runID, logger := d.setCfg, d.stratCfg, d.canLive, d.runID, d.logger

	rep, err := repo.OpenRepo(appSettings.DBPath())
	if err != nil {
		return err
	}
	defer rep.Close()
	d.rep = rep

	if done, err := d.openDay(); done || err != nil {
		return err
	}
	todayJST, today, cal := d.todayJST, d.today, d.cal
	if err := rep.StartRun(runID, todayJST, string(appSettings.Env), d.mode()); err != nil {
		return fmt.Errorf("実行の記録を始められません: %w", err)
	}
	// 途中で返っても実行の終わりを残す（runs.status が running のまま残らないように）
	defer func() { d.finishRun(err) }()

	// 判断の前に足を更新する。cron の data sync とは独立に、
	// この実行が見る足を自分で最新にしてから判断する
	// （--no-sync で抑止。取得元が不調な日に保存済みだけで回すため）。
	if !d.noSync {
		if failures := syncUniverseBars(setCfg, logger, runSyncDays, false, false); failures > 0 {
			logger.Warn("run.sync_failed",
				fmt.Sprintf("%d 銘柄の足を更新できませんでした（保存済みの足で続けます）", failures))
		}
	}

	barStore := data.NewBarStore(appSettings.BarsDir())

	// ブローカー初期化。dry-run は常にメモリ上の模型
	var b broker.Broker
	if !canLive {
		b = broker.NewPaperBroker(decimal.Zero, "open")
	} else if b, err = runBroker(setCfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	bal, err := b.GetBalance()
	if err != nil {
		return err
	}
	equity := bal.CashBalance.Add(bal.MarketValue)
	d.finishEquity, d.finishCash = &equity, &bal.CashBalance
	// 建玉が見えないまま進むと、保有中の銘柄を「未保有」として買い足し、ストップも
	// 現値で作り直してしまう。発注する回は照会に失敗した時点で止める
	posMap, err := b.PositionsBySymbol()
	if err != nil {
		if canLive {
			return fmt.Errorf("建玉を照会できないため発注を中止しました（二重に建てないため）: %w", err)
		}
		logger.Warn("run.positions_failed",
			fmt.Sprintf("建玉を照会できません（dry-run のため未保有として続行）: %v", err))
		posMap = map[string]domain.Position{}
	}
	// その時点の建玉を残す（explain / 事後の検証で「何を持っていたか」を引く）
	if err := rep.RecordSnapshot(runID, todayJST, positionList(posMap)); err != nil {
		logger.Warn("wbjp.ledger", fmt.Sprintf("建玉の記録を残せません: %v", err))
	}

	// 送信結果が分からなかった注文を判定する。決められないものがあれば発注しない
	//（同じ銘柄に二重に出しうる）。dry-run は台帳に PENDING を作らないので飛ばす。
	// 約定の取り込みまで建玉 0 件の確認（checkEmptyPositions）より先に行う（前の回の
	// 売り・買いが約定したかで「持っているはず」が変わる）
	if canLive {
		summary, err := resolvePendingOrders(rep, b, logger, clock.NowUTC())
		if err != nil {
			digest.Anomaly("wbjp.pending_unresolved", err.Error())
			return err
		}
		if summary.Attributed+summary.NotSent+summary.Ambiguous+summary.TooRecent > 0 {
			digest.Note(summary.Fields("pending"))
		}
		if err := pendingBlocksOrders(summary); err != nil {
			return err
		}

		// 出した注文の約定・失効を台帳に取り込む。当日買付（差金決済の柵）と
		// 未約定の買い（比率上限）はこの台帳から数えるので、発注の判断より先に行う。
		// 照会できなかった注文は未確定のまま残る（未約定に数え続けるので安全側）
		fills, err := execute.SyncFills(rep, b)
		if err != nil {
			digest.Anomaly("wbjp.fill_sync_failed", err.Error())
			return err
		}
		for _, c := range fills.Changes {
			logger.Info("wbjp.fill", fmt.Sprintf("%s: %s → %s（%s/%s 株約定, ID: %s）",
				c.Symbol, c.Before, c.After, c.FilledQuantity, c.Quantity, c.ClientOrderID))
		}
		if len(fills.Unresolved) > 0 {
			logger.Warn("wbjp.fill_unresolved", "注文を照会できません（台帳は未確定のまま）:\n"+strings.Join(fills.Unresolved, "\n"))
			digest.Anomaly("wbjp.fill_unresolved", fmt.Sprintf("%d 件の注文を照会できません（次の実行で再照会）", len(fills.Unresolved)))
		}
		if len(fills.Changes) > 0 {
			digest.Note(map[string]any{"phase": "fills", "changed": len(fills.Changes)})
		}
	}

	// 建玉の照会がエラーなしで 0 件なのに、台帳では保有中のはずの銘柄がある。信じると
	// ストップを全部消し、保有中の銘柄を新規として買い直すので、発注する回は止める
	if err := checkEmptyPositions(rep, posMap, string(appSettings.Env), runID, canLive, d.acceptFlat, logger); err != nil {
		return err
	}

	// 1. 日足の収集と ATR / 直近終値
	lastPrices := make(map[string]decimal.Decimal)
	atrMap := make(map[string]decimal.Decimal)
	lotSizes := make(map[string]decimal.Decimal)
	allBars := make(map[string][]domain.Bar)
	// 足が古い・読めない銘柄（銘柄 → 理由）。この回は売りも買いも出さない（W6）
	unusable := make(map[string]string)

	for _, sym := range setCfg.Universe.Symbols {
		lotSizes[sym] = decimal.NewFromInt(100)
		if ov, ok := setCfg.Universe.LotSizeOverrides[sym]; ok && ov > 0 {
			lotSizes[sym] = decimal.NewFromInt(int64(ov))
		}

		bars, err := barStore.Read(sym, "", "")
		if why := barsUnusable(bars, err, today, cal.PreviousTradingDay); why != "" {
			unusable[sym] = why
		}
		if err != nil || len(bars) == 0 {
			continue
		}
		allBars[sym] = bars
		lastBar := bars[len(bars)-1]
		lastPrices[sym] = lastBar.Close

		highs := make([]float64, len(bars))
		lows := make([]float64, len(bars))
		closes := make([]float64, len(bars))
		for i, bar := range bars {
			h, _ := bar.High.Float64()
			l, _ := bar.Low.Float64()
			c, _ := bar.Close.Float64()
			highs[i] = h
			lows[i] = l
			closes[i] = c
		}
		atrVals, _ := indicators.ATR(highs, lows, closes, 14)
		if len(atrVals) > 0 {
			atrMap[sym] = decimal.NewFromFloat(atrVals[len(atrVals)-1])
		}
	}

	// 足が古い・読めない銘柄は、この回は判断しない（売りも買いも出さない）。
	// ストップの判定（損切り・利確）にも使わない（古い足で損切り・利確を決めない）
	decisionCloses := make(map[string]decimal.Decimal, len(lastPrices))
	for sym, px := range lastPrices {
		if _, ng := unusable[sym]; !ng {
			decisionCloses[sym] = px
		}
	}
	reportUnusableBars(unusable, posMap, logger)

	// 2. ストップロスの管理と更新
	// 保存済みのストップが読めないと、全銘柄のストップが現値から作り直される（建値・
	// 最高値の履歴が消える）。読めない台帳で発注しない
	savedStops, err := rep.GetStops()
	if err != nil {
		return fmt.Errorf("ストップの記録を読めません: %w", err)
	}
	stopObjMap := make(map[string]*risk.Stop)
	for sym, st := range savedStops {
		stopObjMap[sym] = &risk.Stop{
			Symbol:           st.Symbol,
			StopPrice:        st.StopPrice,
			EntryPrice:       st.EntryPrice,
			CreatedOn:        st.CreatedOn,
			Trailing:         st.Trailing,
			ATRMultiple:      st.ATRMultiple,
			TrailingPct:      st.TrailingPct,
			HighestClose:     st.HighestClose,
			InitialStopPrice: st.InitialStopPrice,
			InitialQuantity:  st.InitialQuantity,
			ScaledOut:        st.ScaledOut,
		}
	}
	stopBook := risk.NewStopBook(stopObjMap)
	// 手仕舞った銘柄のストップを外す（外すのはここ 1 か所）。発注する回は建玉を確かに
	// 照会できている（照会に失敗したら上で止まる）。dry-run はメモリ上の模型（建玉 0）
	// なので全部外れるが、dry-run はストップを保存しないので台帳は変わらない
	if removed := stopBook.RetainHeld(posMap); canLive && len(removed) > 0 {
		logger.Info("wbjp.stop_removed", fmt.Sprintf("保有していない銘柄のストップを外しました: %s", strings.Join(removed, ", ")))
	}
	stopBook.EnsureWithOptions(posMap, atrMap, todayJST,
		risk.EnsureOptionsFrom(setCfg.Stops, setCfg.Sizing.ATRStopMultiple))
	stopBook.UpdateTrailing(decisionCloses, atrMap)
	// 建値への引き上げは利確・トレーリングより先に行う。
	stopBook.UpdateBreakeven(decisionCloses, setCfg.Stops.BreakevenAfterR)
	// ストップの保存は利確（ScaledOut・建値への引き上げ）を決めた後（3-4）

	// 3. 戦略の評価
	strats, weights, err := buildStrategies(stratCfg)
	if err != nil {
		return err
	}
	combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

	var allSignals []domain.Signal
	var combinedSignals []domain.CombinedSignal
	targets := make(map[string]domain.TargetPosition)
	// wbjp が売買する銘柄。ユニバース外の保有（手で買った株など）には手を出さない
	universe := symbolSet(setCfg.Universe.Symbols)

	// 3-1. 全銘柄のシグナルを出す。
	//
	// 戦略には銘柄ごとではなく全銘柄をまとめて渡す。モメンタムの
	// 順位付けやベンチマークとの比較は、1 銘柄ずつ呼ぶ形では書けない。
	stratUniverse := strategy.NewUniverse(allBars)
	if needsMargin(stratCfg) {
		book, err := loadMarginBook(setCfg.Universe.Symbols)
		if err != nil {
			return err
		}
		if book == nil {
			logger.Warn("wbjp.margin_missing", "信用残がアーカイブにありません。margin_balance は黙ります")
		}
		stratUniverse.SetMargin(book)
	}
	stratCtx := stratUniverse.At(todayJST, posMap, equity)

	signalsBySymbol := make(map[string][]domain.Signal)
	for _, s := range strats {
		sigs, err := s.OnBars(stratCtx)
		if err != nil {
			logger.Warn("wbjp.strategy_error", fmt.Sprintf("%s の評価に失敗: %v", s.Name(), err))
			continue
		}
		for _, sig := range sigs {
			signalsBySymbol[sig.Symbol] = append(signalsBySymbol[sig.Symbol], sig)
			allSignals = append(allSignals, sig)
		}
	}

	signalMap := make(map[string]domain.CombinedSignal)
	for _, sym := range setCfg.Universe.Symbols {
		// 足の無い銘柄は判断材料が無い。合成すると「中立」を主張した
		// ことになり、保有中なら手仕舞い扱いになってしまう。
		if !stratCtx.HasBars(sym, 1) {
			continue
		}
		// 足が古い銘柄も同じ（古い足での「シグナル消滅」で全株を売らない。W6）
		if _, ng := unusable[sym]; ng {
			continue
		}
		combined := combineFunc(sym, signalsBySymbol[sym], weights)
		combinedSignals = append(combinedSignals, combined)
		signalMap[sym] = combined
	}

	// 3-2. 地合いに応じて露出を絞る。弱気なら新規を止めて全て手仕舞う。
	sizingEquity := equity
	if setCfg.Regime.Enabled {
		regimeName, exposure := risk.RegimeExposure(setCfg.Regime, regimeInput(barStore, setCfg.Regime))
		signalMap, sizingEquity = risk.ApplyRegime(regimeName, exposure, signalMap, posMap, equity)
		logger.Info("wbjp.regime",
			fmt.Sprintf("地合い %s: 露出 %s（サイジング基準 %s円）", regimeName, exposure, sizingEquity.Round(0)))
	}

	// 3-3. 保有銘柄数の上限・手仕舞い閾値・再サイジング抑制は
	// ポートフォリオ全体を見ないと決まらないので一括で計算する。
	sizer, err := portfolio.NewSizer(setCfg.Sizing)
	if err != nil {
		return err
	}
	strategyTargets := sizer.Size(signalMap, portfolio.SizingContext{
		Equity:      sizingEquity,
		BuyingPower: bal.BuyingPower,
		Prices:      lastPrices,
		ATR:         atrMap,
		LotSizes:    lotSizes,
		Positions:   posMap,
	}, stratCfg.EntryThreshold, stratCfg.ExitThreshold)

	// 3-4. ストップ由来の手仕舞いを集める。これらは戦略の判断より優先する。
	// 並べ方・重ね方は backtest と同じ risk.ExitPlan（損切りが残り玉の「維持」に負けない）
	quantities := quantitiesOf(posMap)
	stopTargets := decideStopExits(rep, stopBook, setCfg.Stops, risk.ExitInputs{
		Closes: decisionCloses, Quantities: quantities, LotSizes: lotSizes, AsOf: todayJST,
		Bars: func(sym string) []domain.Bar { return allBars[sym] },
		// 時間切れの営業日数は東証のカレンダーで数える（祝日を数えない。読めなければ平日で代用するが、
		// 発注する回はその前の tradingDayGate で止まっている）
		TradingDay: cal.IsTradingDay,
	}, canLive, logger)
	for _, t := range risk.ApplyStopPriority(strategyTargets, stopTargets) {
		if _, ng := unusable[t.Symbol]; ng {
			continue // 判断しない銘柄の目標（シグナルが無いための手仕舞い）を台帳に残さない
		}
		if _, ok := universe[t.Symbol]; !ok {
			continue // ユニバース外の保有（手で買った株など）には手を出さない（地合いの手仕舞いも）
		}
		targets[t.Symbol] = t
	}

	if err := rep.RecordSignals(runID, allSignals); err != nil {
		logger.Warn("wbjp.ledger", fmt.Sprintf("シグナルを記録できません: %v", err))
	}
	if err := rep.RecordCombinedSignals(runID, combinedSignals); err != nil {
		logger.Warn("wbjp.ledger", fmt.Sprintf("合成シグナルを記録できません: %v", err))
	}

	var targetList []domain.TargetPosition
	for _, t := range targets {
		targetList = append(targetList, t)
	}
	if err := rep.RecordTargets(runID, targetList); err != nil {
		logger.Warn("wbjp.ledger", fmt.Sprintf("目標を記録できません: %v", err))
	}

	// 4. リコンサイル
	//
	// 板に残っている注文が見えないと、同じ注文をもう一度出しうる。
	// 発注する回では照会に失敗した時点で止める（dry-run は記録だけ
	// なので、見えないまま続けても実害は無い）。
	openOrders, err := b.GetOpenOrders()
	if err != nil {
		if canLive {
			return fmt.Errorf("板の注文を照会できないため発注を中止しました（二重発注を避けます）: %w", err)
		}
		logger.Warn("run.open_orders_failed",
			fmt.Sprintf("板の注文を照会できません（dry-run のため続行）: %v", err))
		openOrders = nil
	}

	orderType := domain.OrderTypeLimit
	if setCfg.Execution.OrderType == "market" {
		orderType = domain.OrderTypeMarket
	}
	limitOffset := decimal.RequireFromString("0.005")
	if setCfg.Execution.LimitOffset != "" {
		if d, err := decimal.NewFromString(setCfg.Execution.LimitOffset); err == nil {
			limitOffset = d
		}
	}

	taxType := domain.TaxAccountSpecific
	if setCfg.Execution.TaxAccountType != "" {
		taxType = domain.TaxAccountType(setCfg.Execution.TaxAccountType)
	}

	// 当日買い付けた銘柄。現物の差金決済を避けるため売却を止める。
	boughtToday, err := rep.BoughtToday(todayJST)
	if err != nil {
		return fmt.Errorf("当日の買付履歴を読めません: %w", err)
	}

	plan, err := engine.Reconcile(targets, posMap, openOrders, lastPrices, lotSizes,
		engine.ReconcileSettings{
			OrderType:         orderType,
			LimitOffset:       limitOffset,
			TaxType:           taxType,
			Topix500:          symbolSet(setCfg.Universe.TOPIX500Symbols),
			BlocksSameDaySale: true,
			Frozen:            unusable,
			Universe:          universe,
		},
		boughtToday, todayJST)
	if err != nil {
		return err
	}

	for sym, why := range plan.Skipped {
		logger.Info("wbjp.reconcile_skip", fmt.Sprintf("%s 見送り: %s", sym, why))
	}
	if outside := outsideUniverseHeld(posMap, universe); len(outside) > 0 {
		logger.Info("wbjp.outside_universe", "ユニバース外の保有には手を出しません: "+strings.Join(outside, ", "))
		digest.Note(map[string]any{"outside_universe": outside})
	}

	// 5. リスク管理チェック (RiskManager)
	riskMgr := risk.NewRiskManager(setCfg.Risk, setCfg.Universe.Symbols)

	// 未約定の買い注文が押さえている金額と、当日の発注件数は
	// プロセスをまたいで数える。実行ごとに 0 から数え直すと
	// 1日に何度 run しても上限が効かない。
	pendingValue, err := rep.PendingBuyValue(lastPrices)
	if err != nil {
		return fmt.Errorf("未約定注文を読めません: %w", err)
	}
	ordersToday, err := rep.OrdersToday(todayJST)
	if err != nil {
		return fmt.Errorf("当日の発注件数を読めません: %w", err)
	}

	// 当日の損益（max_daily_loss）。以前は 0 固定で、本番では上限が効いていなかった
	// （2026-09-24 のレビュー W5）。確かめられなければ新規の買いを止める
	realized, unrealized, unpriced, err := dailyPnL(rep, todayJST, posMap, lastPrices, boughtToday)
	if err != nil {
		unpriced = append(unpriced, err.Error())
	}
	if len(unpriced) > 0 {
		logger.Warn("wbjp.daily_pnl_unknown", "当日の損益を確かめられないため新規の買いを止めます:\n"+strings.Join(unpriced, "\n"))
		digest.Anomaly("wbjp.daily_pnl_unknown", fmt.Sprintf("%d 件（新規の買いを止めた）", len(unpriced)))
	}
	logger.Info("wbjp.daily_pnl", fmt.Sprintf("当日の損益: 実現 %s 円・含み %s 円（上限 %s 円）",
		realized.Round(0), unrealized.Round(0), setCfg.Risk.MaxDailyLoss))

	riskCtx := &risk.RiskContext{
		Equity:             equity,
		Balance:            *bal,
		Positions:          posMap,
		BasePrices:         lastPrices,
		PendingValue:       pendingValue,
		OrdersToday:        ordersToday,
		RealizedPnLToday:   realized,
		UnrealizedPnLToday: unrealized,
		DailyPnLUnknown:    len(unpriced) > 0,
	}

	var requests []domain.OrderRequest
	for _, res := range plan.Orders {
		if res.Request != nil {
			requests = append(requests, *res.Request)
		}
	}
	// 発注済みの確認・リスク審査・送信・台帳の更新と、受理したぶんの余力の差し引きは
	// execute.PlaceOrders（テストあり）。売りを先に並べる（max_orders_per_day を買いで
	// 使い切って損切りを見送らない。risk.SellsFirst。backtest も同じ関数を通す）
	result, err := execute.PlaceOrders(rep, b, risk.SellsFirst(requests), riskCtx, execute.Options{
		RunID: runID, Live: canLive, Risk: riskMgr, Report: logger,
	})
	// 見送りの理由は発注が途中で止まっても残す（explain で引く）
	if rerr := rep.RecordRiskEvents(runID, result.RiskRejected); rerr != nil {
		logger.Warn("wbjp.ledger", fmt.Sprintf("リスクの見送りを記録できません: %v", rerr))
	}
	if err != nil {
		var unconfirmed *execute.ErrUnconfirmedOrder
		if errors.As(err, &unconfirmed) {
			return fmt.Errorf("注文 %s の結果を確認できないため発注を中止しました（口座と台帳 %s を確かめてください）: %w",
				unconfirmed.ClientOrderID, appSettings.DBPath(), err)
		}
		return err
	}
	if len(result.Failed) > 0 {
		digest.Anomaly("wbjp.order_failed", fmt.Sprintf("%d 件の注文を受け付けられませんでした:\n%s",
			len(result.Failed), strings.Join(result.Failed, "\n")))
	}

	liveOrders, dryRunOrders := result.Placed, 0
	if !canLive {
		liveOrders, dryRunOrders = 0, result.Placed
	}
	digest.Note(map[string]any{"phase": "run", "live": canLive, "orders": liveOrders, "dry_run_orders": dryRunOrders,
		"risk_rejected": len(result.RiskRejected), "targets": len(targetList)})
	return nil
}

var decimalZero = decimal.Zero

// dailyRun は日次実行 1 回ぶんの状態。runDaily の段（準備・接続・照合・判断・発注）が順に埋める。
type dailyRun struct {
	// 準備（prepareDaily）
	noSync, acceptFlat bool
	setCfg             *wbjpcfg.SettingsFile
	stratCfg           *wbjpcfg.StrategiesConfig
	canLive            bool
	// ロガーと run_id（run_id は入口で発行済み。ログ・DB・履歴で共有する）
	runID  string
	logger *logging.Logger
	rep    *repo.Repo

	// 判定日（openDay）。営業日と「あるべき最後の足」は東証のカレンダーで決める
	todayJST string
	today    time.Time
	cal      *calendar.Calendar

	// 実行の終わりに残す評価額・現金（照会できた時点で埋まる）
	finishEquity, finishCash *decimal.Decimal
}

// prepareDaily は設定を読み、発注するかを決めて口座を表示し、本番発注なら確認を取る。
func prepareDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag, acceptFlatFlag bool) (*dailyRun, error) {
	// 以降のログの全行とダイジェストに印を付ける（env とは独立）
	run.SetVerify(brokerVerifyFlag)
	setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
	if err != nil {
		return nil, err
	}
	stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
	if err != nil {
		return nil, err
	}

	canLive, reason := appSettings.CanExecuteLive(liveFlag, setCfg.Risk.KillSwitch)
	envName := "テスト口座 (UAT)"
	if appSettings.Env.IsProduction() {
		envName = "本番口座 (PROD)"
	}
	fmt.Printf("口座: %s (WBJP_ENV=%s)  発注: %s (%s)\n\n", envName, appSettings.Env, func() string {
		if canLive {
			return "する"
		}
		return "しない"
	}(), reason)

	if err := cli.ConfirmLive(appSettings, canLive, yesFlag); err != nil {
		return nil, err
	}

	return &dailyRun{
		noSync: noSyncFlag, acceptFlat: acceptFlatFlag,
		setCfg: setCfg, stratCfg: stratCfg, canLive: canLive,
		runID: run.RunID, logger: run.Logger,
	}, nil
}

// openDay は今日（JST）を決め、休場日なら判断も発注もせずに終える（done）。
// カレンダーが読めなければ発注する回は止める（tradingDayGate）。
func (d *dailyRun) openDay() (done bool, err error) {
	d.todayJST = clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02")
	d.today, err = time.Parse("2006-01-02", d.todayJST)
	if err != nil {
		return false, fmt.Errorf("今日の日付を読めません: %w", err)
	}
	// 営業日と「あるべき最後の足」は東証のカレンダーで決める
	d.cal = calendar.FromArchive(archive.NewArchive(appSettings.JQuantsArchiveDir()))
	if skip, err := tradingDayGate(d.cal, d.today, d.canLive); err != nil {
		return false, err
	} else if skip != "" {
		d.logger.Info("wbjp.market_closed", skip)
		fmt.Println(skip)
		return true, nil
	}
	if d.cal.Empty() {
		d.logger.Warn("wbjp.calendar_missing", "取引カレンダーが読めないので平日を営業日とみなします（祝日明けは足が古いとみなして止まる）")
	}
	return false, nil
}

// mode は runs.mode に残す実行の形（live / dry_run）。
func (d *dailyRun) mode() string {
	if d.canLive {
		return "live"
	}
	return "dry_run"
}

// finishRun は実行の終わりを台帳に残す（err があれば failed）。評価額・現金は照会できた時点で
// 埋まっている（照会の前に止まった回は空）。
func (d *dailyRun) finishRun(err error) {
	status := "success"
	var errText *string
	if err != nil {
		status = "failed"
		s := err.Error()
		errText = &s
	}
	if ferr := d.rep.FinishRun(d.runID, status, d.finishEquity, d.finishCash, errText); ferr != nil {
		d.logger.Warn("wbjp.ledger", fmt.Sprintf("実行の終了を記録できません: %v", ferr))
	}
}

// dailyPnL は当日の損益を実現（台帳の約定した売り）と含み（保有中の建玉の当日の値動き）に分けて返す。
//
// 含みの基準は、当日買い付けた銘柄なら取得単価、それ以外は判断に使う直近の終値
// （場中は前営業日の終値。足が古い銘柄ではその古い終値からの値動き）。
// 現値（LastPrice）が無い・終値が無い（足が無い）建玉は数えない。
// unpriced は実現損益に入れられなかった売り（1 件でもあれば当日の損益は確かでない）。
func dailyPnL(rep *repo.Repo, todayJST string, positions map[string]domain.Position,
	lastPrices map[string]decimal.Decimal, boughtToday map[string]struct{},
) (realized, unrealized decimal.Decimal, unpriced []string, err error) {
	unrealized = decimal.Zero
	for sym, pos := range positions {
		if !pos.Quantity.IsPositive() || !pos.LastPrice.IsPositive() {
			continue
		}
		ref, ok := lastPrices[sym]
		if _, bought := boughtToday[sym]; bought && pos.CostPrice.IsPositive() {
			ref, ok = pos.CostPrice, true
		}
		if !ok || !ref.IsPositive() {
			continue
		}
		unrealized = unrealized.Add(pos.LastPrice.Sub(ref).Mul(pos.Quantity))
	}
	r, err := rep.RealizedPnLOn(todayJST)
	if err != nil {
		return decimal.Zero, unrealized, nil, fmt.Errorf("当日の実現損益を読めません: %w", err)
	}
	return r.Amount, unrealized, r.Unpriced, nil
}

// outsideUniverseHeld はユニバース外で保有している銘柄を昇順で返す。
func outsideUniverseHeld(posMap map[string]domain.Position, universe map[string]struct{}) []string {
	var out []string
	for sym, pos := range posMap {
		if _, ok := universe[sym]; !ok && pos.Quantity.IsPositive() {
			out = append(out, sym)
		}
	}
	sort.Strings(out)
	return out
}

// checkEmptyPositions は、建玉の照会がエラーなしで 0 件を返したのに、台帳では保有中の
// はずの銘柄（repo.ExpectedHoldings）があるかを確かめる。
//
// 照会の不調で 0 件が返ったのを信じると、全銘柄が未保有扱いになり、RetainHeld が
// ストップを全部外し、SyncStops が台帳の stops を全部消し、sizer が保有中の銘柄を新規と
// して買い直す（2026-09-24 の再点検）。発注する回は止める（通知・ダイジェスト・非 0 終了）。
// dry-run は建玉 0 の模型で判断するので警告だけで続ける（ストップは保存しない）。
// acceptFlat（--accept-flat）は口座が本当に空だと確かめたときの逃げ道: 手で全部売った
// 後など、台帳の保有を捨てて続ける（ストップは RetainHeld で外れ、成功した回が次の基準になる）。
// 素通りした回に印を付け、1 回で失効させる（repo.AcceptFlatSpent: 同じ日にもう一度、または
// 素通りして成功した回の直後の回では使えない）。期待が空なら印は付けない。
func checkEmptyPositions(rep *repo.Repo, posMap map[string]domain.Position, env, runID string,
	canLive, acceptFlat bool, logger *logging.Logger) error {
	for _, pos := range posMap {
		if pos.Quantity.IsPositive() {
			return nil
		}
	}
	expected, err := rep.ExpectedHoldings(env, runID)
	if err != nil {
		if canLive {
			return fmt.Errorf("建玉が 0 件と返り、台帳の保有も確かめられないため発注を中止しました: %w", err)
		}
		logger.Warn("wbjp.positions_empty", fmt.Sprintf("台帳の保有を確かめられません（dry-run のため続行）: %v", err))
		return nil
	}
	if len(expected) == 0 {
		return nil
	}
	syms := make([]string, 0, len(expected))
	for sym := range expected {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	lines := make([]string, 0, len(syms))
	for _, sym := range syms {
		lines = append(lines, fmt.Sprintf("%s: %s", sym, expected[sym]))
	}
	detail := strings.Join(lines, "\n")
	if !canLive {
		logger.Warn("wbjp.positions_empty",
			"dry-run は建玉 0 の模型で判断します（台帳では保有中のはず。ストップは保存しない）:\n"+detail)
		return nil
	}
	if acceptFlat {
		// 1 回で失効させる（cron の行に残っても毎回は素通りしない）
		spent, err := rep.AcceptFlatSpent(env, runID)
		if err != nil {
			return fmt.Errorf("建玉が 0 件と返り、--accept-flat を使えるか確かめられないため発注を中止しました: %w", err)
		}
		if spent == "" {
			if err := rep.MarkAcceptFlat(runID); err != nil {
				return fmt.Errorf("--accept-flat の印を残せないため発注を中止しました（1 回で失効させられない）: %w", err)
			}
			logger.Warn("wbjp.positions_empty",
				"建玉の照会は 0 件。--accept-flat のため台帳の保有を捨てて続けます:\n"+detail)
			digest.Note(map[string]any{"phase": "accept_flat", "dropped": len(syms)})
			return nil
		}
		digest.Anomaly("wbjp.positions_empty", fmt.Sprintf("%d 銘柄（発注を中止。%s）:\n%s", len(syms), spent, detail))
		return fmt.Errorf("建玉の照会が 0 件なのに台帳では %d 銘柄を保有中のはずのため発注を中止しました"+
			"（%s。--accept-flat は 1 回で失効します。cron の行に付けていないか確かめてください）:\n%s",
			len(syms), spent, detail)
	}
	digest.Anomaly("wbjp.positions_empty", fmt.Sprintf("%d 銘柄（発注を中止）:\n%s", len(syms), detail))
	return fmt.Errorf("建玉の照会が 0 件なのに台帳では %d 銘柄を保有中のはずのため発注を中止しました"+
		"（口座を確かめ、本当に空なら --accept-flat を付けて 1 回実行してください）:\n%s", len(syms), detail)
}

// decideStopExits はストップ由来の目標を決め（risk.ExitPlan。backtest と同じ）、その後で
// ストップを保存する。
//
// 保存は利確で変えたストップ（建値への引き上げ・ScaledOut）まで含めるため ExitPlan の後
// （2026-09-24 のレビュー W2: 以前は利確の前に保存していて、変更が次の回に残らなかった）。
// 利確の ScaledOut は、保有が利確後の株数まで減ったのを見た回に立つ（AssumeFilled は偽）。
// 売りを出しただけの回では立たないので、約定しなかった利確は次の回に出し直される。
// 発注する回は台帳を StopBook に揃える（外した銘柄の行も消す）。dry-run は保存しない
// （建玉 0 の模型で決めたストップで本番の台帳を書き換えない）。
//
// 保存に失敗しても発注は止めない（警告とダイジェストの異常だけ。2026-09-24 の再点検で
// 検討した）。この回の判断はメモリ上の StopBook（正しい）で決まっていて、止めても失った
// 変更（トレーリングの最高値・ScaledOut・外した銘柄）は戻らない。止めて得るものは無く、
// この回の損切り・手仕舞いの売りまで出なくなる。失ったものは次の回に次のように戻る:
// 最高値はその回の終値から引き上げ直し（ストップは下がらない）、ScaledOut は保有が
// 減ったのを見て立て直し（含み益が take_profit_r 以上の回。TakeProfitTargets と同じ遅れ）、
// 外すはずだったストップは RetainHeld がもう一度外す（その間に同じ銘柄を建て直さない限り）。
// 台帳そのものが壊れて書けないなら、発注前の記録（PlaceRecorded の PENDING）で止まる。
func decideStopExits(rep *repo.Repo, book *risk.StopBook, cfg wbjpcfg.StopsConfig, in risk.ExitInputs,
	canLive bool, logger *logging.Logger) []domain.TargetPosition {
	targets := book.ExitPlan(cfg, in)
	for _, t := range targets {
		if t.Quantity.LessThan(in.Quantities[t.Symbol]) {
			logger.Warn("wbjp.stop_exit", fmt.Sprintf("%s: %s", t.Symbol, t.Reason))
		}
	}
	if canLive {
		if err := rep.SyncStops(stopRecordsOf(book)); err != nil {
			logger.Warn("wbjp.stop_save_failed", fmt.Sprintf("ストップを保存できません: %v", err))
			digest.Anomaly("wbjp.stop_save_failed", err.Error())
		}
	}
	return targets
}
