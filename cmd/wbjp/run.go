package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
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

			for _, sym := range setCfg.Universe.Symbols {
				lotSizes[sym] = decimal.NewFromInt(100)
				if ov, ok := setCfg.Universe.LotSizeOverrides[sym]; ok && ov > 0 {
					lotSizes[sym] = decimal.NewFromInt(int64(ov))
				}

				bars, err := barStore.Read(sym, "", "")
				if err != nil || len(bars) == 0 {
					continue
				}
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
			stopBook.Ensure(posMap, atrMap, todayJST, setCfg.Sizing.ATRStopMultiple, true)
			stopBook.UpdateTrailing(lastPrices, atrMap)

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
			strats, weights := buildStrategies(stratCfg)
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			var allSignals []domain.Signal
			var combinedSignals []domain.CombinedSignal
			targets := make(map[string]domain.TargetPosition)

			// ストップ抵触による手仕舞いを最優先で目標建玉（0株）に設定
			for _, exitTarget := range stopBook.ExitTargets(lastPrices) {
				targets[exitTarget.Symbol] = exitTarget
				logger.Warn("wbjp.stop_exit", fmt.Sprintf("%s: %s", exitTarget.Symbol, exitTarget.Reason))
			}

			for _, sym := range setCfg.Universe.Symbols {
				if _, hasExit := targets[sym]; hasExit {
					continue
				}

				bars, err := barStore.Read(sym, "", "")
				if err != nil || len(bars) == 0 {
					continue
				}
				lastBar := bars[len(bars)-1]

				var sigs []domain.Signal
				for _, s := range strats {
					sig, err := s.OnBars(sym, bars)
					if err == nil && sig != nil {
						sigs = append(sigs, *sig)
						allSignals = append(allSignals, *sig)
					}
				}

				combined := combineFunc(sym, sigs, weights)
				combinedSignals = append(combinedSignals, combined)

				if combined.Direction >= stratCfg.EntryThreshold {
					target, err := portfolio.SizePosition(combined, equity, lastBar.Close, atrMap[sym], lotSizes[sym], setCfg.Sizing)
					if err == nil {
						targets[sym] = target
					}
				}
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

			reconciled, err := engine.Reconcile(targets, posMap, openOrders, lastPrices, lotSizes, orderType, limitOffset, todayJST)
			if err != nil {
				return err
			}

			// 5. リスク管理チェック (RiskManager)
			riskMgr := risk.NewRiskManager(setCfg.Risk, setCfg.Universe.Symbols)
			riskCtx := risk.RiskContext{
				Equity:           equity,
				Balance:          *bal,
				Positions:        posMap,
				BasePrices:       lastPrices,
				PendingValue:     make(map[string]decimal.Decimal),
				OrdersToday:      0,
				RealizedPnLToday: decimal.Zero,
			}

			for _, res := range reconciled {
				if res.Request == nil {
					continue
				}
				req := *res.Request

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

				ack, err := b.Place(req)
				if err != nil {
					logger.Error("wbjp.order_failed", fmt.Sprintf("%s 発注拒否: %v", req.Symbol, err))
					continue
				}

				_ = rep.RecordOrder(runID, req, string(ack.Status), ack.BrokerOrderID)
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
