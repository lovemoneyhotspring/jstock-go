package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/engine"
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

	cmd := &cobra.Command{
		Use:   "run",
		Short: "日次実行サイクルを実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
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

			if canLive && !yesFlag {
				fmt.Print("本番注文を送信します。続行しますか？ [y/N]: ")
				var input string
				_, _ = fmt.Scanln(&input)
				if input != "y" && input != "Y" {
					fmt.Println("中止しました。")
					return nil
				}
			}

			// ロガーとリポジトリ
			runID := fmt.Sprintf("wbjp-%d", time.Now().Unix())
			logger, err := logging.NewLogger("wbjp", string(appSettings.Env), runID, "run", appSettings.LogDir)
			if err != nil {
				return err
			}
			defer logger.Close()

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
			_ = rep.StartRun(runID, todayJST, string(appSettings.Env), mode)

			barStore := data.NewBarStore(appSettings.BarsDir())

			// ブローカー初期化
			var b broker.Broker
			if setCfg.Execution.Broker == "paper" || !canLive {
				b = broker.NewPaperBroker(decimal.Zero, "open")
			} else {
				creds, err := credentials.LoadTachibanaCredentials(appSettings.Env, appSettings.DotenvMap)
				if err != nil {
					return err
				}
				b, err = broker.NewTachibanaBroker(appSettings.Env, creds, appSettings.StateDir)
				if err != nil {
					return err
				}
			}

			bal, err := b.GetBalance()
			if err != nil {
				return err
			}
			equity := bal.CashBalance.Add(bal.MarketValue)
			posMap, _ := b.PositionsBySymbol()

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
			savedStops, _ := rep.GetStops()
			stopObjMap := make(map[string]*risk.Stop)
			for sym, st := range savedStops {
				stopObjMap[sym] = &risk.Stop{
					Symbol:           st.Symbol,
					StopPrice:        st.StopPrice,
					EntryPrice:       st.EntryPrice,
					CreatedOn:        st.CreatedOn,
					Trailing:         st.Trailing,
					ATRMultiple:      st.ATRMultiple,
					HighestClose:     st.HighestClose,
					InitialStopPrice: st.InitialStopPrice,
					InitialQuantity:  st.InitialQuantity,
					ScaledOut:        st.ScaledOut,
				}
			}
			stopBook := risk.NewStopBook(stopObjMap)
			stopBook.EnsureWithOptions(posMap, atrMap, todayJST,
				risk.EnsureOptionsFrom(setCfg.Stops, setCfg.Sizing.ATRStopMultiple))
			stopBook.UpdateTrailing(lastPrices, atrMap)
			// 建値への引き上げは利確・トレーリングより先に行う。
			stopBook.UpdateBreakeven(lastPrices, setCfg.Stops.BreakevenAfterR)

			// DB へのストップ保存
			for sym, st := range stopBook.All() {
				_ = rep.SaveStop(repo.StopRecord{
					Symbol:           sym,
					StopPrice:        st.StopPrice,
					EntryPrice:       st.EntryPrice,
					CreatedOn:        st.CreatedOn,
					Trailing:         st.Trailing,
					ATRMultiple:      st.ATRMultiple,
					HighestClose:     st.HighestClose,
					InitialStopPrice: st.InitialStopPrice,
					InitialQuantity:  st.InitialQuantity,
					ScaledOut:        st.ScaledOut,
				})
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
			stratCtx := strategy.NewContext(todayJST, allBars, posMap, equity)

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

			_ = rep.RecordSignals(runID, allSignals)
			_ = rep.RecordCombinedSignals(runID, combinedSignals)

			var targetList []domain.TargetPosition
			for _, t := range targets {
				targetList = append(targetList, t)
			}
			_ = rep.RecordTargets(runID, targetList)

			// 4. リコンサイル
			openOrders, _ := b.GetOpenOrders()

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

			riskCtx := risk.RiskContext{
				Equity:           equity,
				Balance:          *bal,
				Positions:        posMap,
				BasePrices:       lastPrices,
				PendingValue:     pendingValue,
				OrdersToday:      ordersToday,
				RealizedPnLToday: decimal.Zero,
			}

			for _, res := range plan.Orders {
				if res.Request == nil {
					continue
				}
				req := *res.Request

				// 送信後・記録前に落ちた注文を再送しない。
				if rep.WasPlaced(req.ClientOrderID) {
					logger.Info("wbjp.skip", fmt.Sprintf("%s: 既に発注済み (ID: %s)", req.Symbol, req.ClientOrderID))
					continue
				}

				decision := riskMgr.Check(req, riskCtx, nil)
				if !decision.Approved {
					logger.Warn("wbjp.risk_rejected", fmt.Sprintf("%s 発注見送り: %s", req.Symbol, decision.Reason))
					continue
				}

				if !canLive {
					_ = rep.RecordOrder(runID, req, "dry_run", nil)
					logger.Info("wbjp.dry_run", fmt.Sprintf("[dry-run] %s %s %s株 @ %s円 (%s)", req.Symbol, req.Side, req.Quantity, req.LimitPrice, req.Reason))
					continue
				}

				// 送信前に記録する。応答が返らなくても次回の再送を止める。
				if err := rep.RecordOrder(runID, req, string(domain.OrderStatusPending), nil); err != nil {
					return fmt.Errorf("発注前の記録に失敗しました（発注を中止します）: %w", err)
				}

				ack, err := b.Place(req)
				if err != nil {
					var rejected *broker.OrderRejectedError
					if errors.As(err, &rejected) {
						_ = rep.UpdateOrder(req.ClientOrderID, domain.OrderStatusRejected, decimal.Zero, nil, nil)
						logger.Error("wbjp.order_failed", fmt.Sprintf("%s 発注拒否: %v", req.Symbol, err))
						continue
					}
					// 届いたか分からない。送信中のまま残して人に確かめてもらう。
					logger.Error("wbjp.unconfirmed",
						fmt.Sprintf("%s 注文 %s の結果を確認できません（送信済みの可能性）: %v", req.Symbol, req.ClientOrderID, err))
					return fmt.Errorf("注文 %s の結果を確認できませんでした: %w", req.ClientOrderID, err)
				}

				_ = rep.UpdateOrder(req.ClientOrderID, ack.Status, decimal.Zero, nil, ack.BrokerOrderID)
				logger.Info("wbjp.order", fmt.Sprintf("発注成功: %s %s %s株 (ID: %s)", req.Symbol, req.Side, req.Quantity, req.ClientOrderID))
				riskCtx.OrdersToday++
			}

			_ = rep.FinishRun(runID, "success", &equity, &bal.CashBalance, nil)
			return nil
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmd.Flags().BoolVar(&yesFlag, "yes", false, "本番発注時の確認プロンプトをスキップする")
	return cmd
}
