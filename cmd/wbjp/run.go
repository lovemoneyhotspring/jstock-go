package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/digest"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
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

	cmd := &cobra.Command{
		Use:   "run",
		Short: "日次実行サイクルを実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.Crash("日次実行", "wbjp.crash",
				runDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag))
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	cmd.Flags().BoolVar(&noSyncFlag, "no-sync", false, "足の更新をしない")
	cmd.Flags().BoolVar(&brokerVerifyFlag, "broker-verify", false,
		"発注経路の実機検証（docs/BROKER_VERIFY.md）。ログとダイジェストに印を付ける")
	return cmd
}

// runDaily は本体。RunE から切り出してあるのは、異常終了を run.Crash で記録・通知するため。
func runDaily(liveFlag, yesFlag, noSyncFlag, brokerVerifyFlag bool) (err error) {
	// 以降のログの全行とダイジェストに印を付ける（env とは独立）
	run.SetVerify(brokerVerifyFlag)
	setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
	if err != nil {
		return err
	}
	stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
	if err != nil {
		return err
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
		return err
	}

	// ロガーとリポジトリ（run_id は入口で発行済み。ログ・DB・履歴で共有する）
	runID := run.RunID
	logger := run.Logger

	rep, err := repo.OpenRepo(appSettings.DBPath())
	if err != nil {
		return err
	}
	defer rep.Close()

	todayJST := clock.ToZone(clock.NowUTC(), clock.Tokyo).Format("2006-01-02")
	mode := "dry_run"
	if canLive {
		mode = "live"
	}
	if err := rep.StartRun(runID, todayJST, string(appSettings.Env), mode); err != nil {
		return fmt.Errorf("実行の記録を始められません: %w", err)
	}
	// 途中で返っても実行の終わりを残す（runs.status が running のまま残らないように）。
	// 評価額・現金は照会できた時点で埋まる
	var finishEquity, finishCash *decimal.Decimal
	defer func() {
		status := "success"
		var errText *string
		if err != nil {
			status = "failed"
			s := err.Error()
			errText = &s
		}
		if ferr := rep.FinishRun(runID, status, finishEquity, finishCash, errText); ferr != nil {
			logger.Warn("wbjp.ledger", fmt.Sprintf("実行の終了を記録できません: %v", ferr))
		}
	}()

	// 判断の前に足を更新する。cron の data sync とは独立に、
	// この実行が見る足を自分で最新にしてから判断する
	// （--no-sync で抑止。取得元が不調な日に保存済みだけで回すため）。
	if !noSyncFlag {
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
	} else if b, err = run.ConnectBroker(setCfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	bal, err := b.GetBalance()
	if err != nil {
		return err
	}
	equity := bal.CashBalance.Add(bal.MarketValue)
	finishEquity, finishCash = &equity, &bal.CashBalance
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

	// 1. 日足の収集と ATR / 直近終値
	lastPrices := make(map[string]decimal.Decimal)
	atrMap := make(map[string]decimal.Decimal)
	lotSizes := make(map[string]decimal.Decimal)
	allBars := make(map[string][]domain.Bar)

	for _, sym := range setCfg.Universe.Symbols {
		lotSizes[sym] = decimal.NewFromInt(100)
		if ov, ok := setCfg.Universe.LotSizeOverrides[sym]; ok && ov > 0 {
			lotSizes[sym] = decimal.NewFromInt(int64(ov))
		}

		bars, err := barStore.Read(sym, "", "")
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
	// 手仕舞った銘柄のストップを外す。建玉を確かに照会できた回（発注する回）だけ——
	// dry-run はメモリ上の模型（建玉 0）なので、ここで外すと全銘柄のストップを失う
	if canLive {
		if removed := stopBook.RetainHeld(posMap); len(removed) > 0 {
			logger.Info("wbjp.stop_removed", fmt.Sprintf("保有していない銘柄のストップを外しました: %s", strings.Join(removed, ", ")))
		}
	}
	stopBook.EnsureWithOptions(posMap, atrMap, todayJST,
		risk.EnsureOptionsFrom(setCfg.Stops, setCfg.Sizing.ATRStopMultiple))
	stopBook.UpdateTrailing(lastPrices, atrMap)
	// 建値への引き上げは利確・トレーリングより先に行う。
	stopBook.UpdateBreakeven(lastPrices, setCfg.Stops.BreakevenAfterR)

	// DB へのストップ保存。発注する回は台帳を StopBook に揃える（外した銘柄の行も消す）
	stopRecords := make(map[string]repo.StopRecord)
	for sym, st := range stopBook.All() {
		stopRecords[sym] = repo.StopRecord{
			Symbol:           sym,
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
	if canLive {
		if err := rep.SyncStops(stopRecords); err != nil {
			logger.Warn("wbjp.stop_save_failed", fmt.Sprintf("ストップを保存できません: %v", err))
		}
	} else {
		for sym, rec := range stopRecords {
			if err := rep.SaveStop(rec); err != nil {
				logger.Warn("wbjp.stop_save_failed", fmt.Sprintf("%s: ストップを保存できません: %v", sym, err))
			}
		}
	}

	// 3. 戦略の評価
	strats, weights, err := buildStrategies(stratCfg)
	if err != nil {
		return err
	}
	combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

	var allSignals []domain.Signal
	var combinedSignals []domain.CombinedSignal
	targets := make(map[string]domain.TargetPosition)

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
	quantities := quantitiesOf(posMap)
	stopTargets := stopBook.ExitTargets(lastPrices)
	stopTargets = append(stopTargets,
		stopBook.TimeExitTargets(lastPrices, todayJST, setCfg.Stops.StaleExitDays, setCfg.Stops.MaxHoldDays)...)
	stopTargets = append(stopTargets,
		stopBook.TakeProfitTargets(lastPrices, quantities, lotSizes,
			setCfg.Stops.TakeProfitR, setCfg.Stops.TakeProfitFraction, marketrules.DefaultLotSize)...)
	stopTargets = append(stopTargets,
		stopBook.RunnerTargets(lastPrices, quantities,
			trendValues(barStore, setCfg.Universe.Symbols, setCfg.Stops), setCfg.Stops.TrendExitAlways)...)

	for _, t := range risk.ApplyStopPriority(strategyTargets, stopTargets) {
		targets[t.Symbol] = t
	}
	for _, t := range stopTargets {
		logger.Warn("wbjp.stop_exit", fmt.Sprintf("%s: %s", t.Symbol, t.Reason))
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

	// 3-5. 送信結果が分からなかった注文を判定する。決められないものがあれば発注しない
	//（同じ銘柄に二重に出しうる）。dry-run は台帳に PENDING を作らないので飛ばす
	if canLive {
		summary, err := resolvePendingOrders(rep, b, logger, clock.NowUTC())
		if err != nil {
			digest.Anomaly("wbjp.pending_unresolved", err.Error())
			return err
		}
		if summary.Attributed+summary.NotSent+summary.Ambiguous+summary.TooRecent > 0 {
			digest.Note(summary.Fields("pending"))
		}
		if summary.Ambiguous > 0 {
			digest.Anomaly("wbjp.pending_ambiguous", fmt.Sprintf("%d 件の送信結果不明の注文を自動で決められません", summary.Ambiguous))
			return fmt.Errorf("送信結果不明の注文 %d 件を決められないため発注を中止しました（二重発注を避けます）", summary.Ambiguous)
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
		},
		boughtToday, todayJST)
	if err != nil {
		return err
	}

	for sym, why := range plan.Skipped {
		logger.Info("wbjp.reconcile_skip", fmt.Sprintf("%s 見送り: %s", sym, why))
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

	riskCtx := &risk.RiskContext{
		Equity:           equity,
		Balance:          *bal,
		Positions:        posMap,
		BasePrices:       lastPrices,
		PendingValue:     pendingValue,
		OrdersToday:      ordersToday,
		RealizedPnLToday: decimal.Zero,
	}

	var requests []domain.OrderRequest
	for _, res := range plan.Orders {
		if res.Request != nil {
			requests = append(requests, *res.Request)
		}
	}
	// 発注済みの確認・リスク審査・送信・台帳の更新と、受理したぶんの余力の差し引きは
	// execute.PlaceOrders（テストあり）
	result, err := execute.PlaceOrders(rep, b, requests, riskCtx, execute.Options{
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
