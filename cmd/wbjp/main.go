package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/indicators"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/engine"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/portfolio"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/risk"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/strategy"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func buildStrategies(stratCfg *wbjpcfg.StrategiesConfig) ([]strategy.Strategy, map[string]float64) {
	var strats []strategy.Strategy
	weights := make(map[string]float64)

	for _, se := range stratCfg.Strategies {
		if !se.IsEnabled() {
			continue
		}
		weights[se.Name] = se.Weight
		switch se.Name {
		case "sma_cross":
			strats = append(strats, strategy.NewSMACross(se.Fast, se.Slow))
		case "rsi_reversion":
			strats = append(strats, strategy.NewRSIReversion(se.Period, 30, 70, 40))
		case "atr_breakout":
			strats = append(strats, strategy.NewATRBreakout(20, 14, 0.005))
		case "trend_pullback":
			strats = append(strats, strategy.NewTrendPullback())
		}
	}
	return strats, weights
}

func main() {
	appSettings := settings.LoadAppSettings()

	var rootCmd = &cobra.Command{
		Use:   "wbjp",
		Short: "日本株スイング売買システム（立花証券 e支店 API / Paper）",
	}

	var liveFlag bool
	var yesFlag bool
	var configDirFlag string

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", appSettings.ConfigDir, "設定ディレクトリ")

	// account サブコマンド
	var cmdAccount = &cobra.Command{
		Use:   "account",
		Short: "口座残高と保有建玉を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			var b broker.Broker
			if setCfg.Execution.Broker == "paper" {
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
				return fmt.Errorf("残高照会エラー: %w", err)
			}

			positions, err := b.GetPositions()
			if err != nil {
				return fmt.Errorf("建玉照会エラー: %w", err)
			}

			fmt.Printf("=== 口座情報 (%s: %s) ===\n", setCfg.Execution.Broker, appSettings.Env)
			fmt.Printf("現金残高: %s 円\n", bal.CashBalance)
			fmt.Printf("買付余力: %s 円\n", bal.BuyingPower)
			fmt.Printf("建玉評価額: %s 円\n\n", bal.MarketValue)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "銘柄\t数量\t取得単価\t現在値\t評価損益")
			for _, pos := range positions {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					pos.Symbol, pos.Quantity, pos.CostPrice, pos.LastPrice, pos.UnrealizedPnL())
			}
			w.Flush()
			return nil
		},
	}

	// stops サブコマンド
	var cmdStops = &cobra.Command{
		Use:   "stops",
		Short: "ストップロスの現在状況を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			rep, err := repo.OpenRepo(appSettings.DBPath())
			if err != nil {
				return err
			}
			defer rep.Close()

			stopMap, err := rep.GetStops()
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "銘柄\tストップ価格\t建値\tATR倍率\t最高終値\t作成日")
			for sym, st := range stopMap {
				hc := "-"
				if st.HighestClose != nil {
					hc = st.HighestClose.String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					sym, st.StopPrice, st.EntryPrice, st.ATRMultiple, hc, st.CreatedOn)
			}
			w.Flush()
			return nil
		},
	}

	// screen サブコマンド
	var cmdScreen = &cobra.Command{
		Use:   "screen",
		Short: "戦略の合致度による銘柄順位表を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
			if err != nil {
				return err
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			strats, weights := buildStrategies(stratCfg)
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			type screenItem struct {
				symbol    string
				direction float64
				reason    string
			}
			var items []screenItem

			for _, sym := range setCfg.Universe.Symbols {
				bars, err := barStore.Read(sym, "", "")
				if err != nil || len(bars) == 0 {
					continue
				}

				var sigs []domain.Signal
				for _, s := range strats {
					sig, err := s.OnBars(sym, bars)
					if err == nil && sig != nil {
						sigs = append(sigs, *sig)
					}
				}

				combined := combineFunc(sym, sigs, weights)
				items = append(items, screenItem{
					symbol:    sym,
					direction: combined.Direction,
					reason:    combined.Reason,
				})
			}

			sort.Slice(items, func(i, j int) bool {
				return items[i].direction > items[j].direction
			})

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "順位\t銘柄\t合成スコア\t判定")
			for i, item := range items {
				judgment := "中立"
				if item.direction >= stratCfg.EntryThreshold {
					judgment = "★ 買い建て候補"
				} else if item.direction <= -stratCfg.ExitThreshold {
					judgment = "▼ 手仕舞い候補"
				}
				fmt.Fprintf(w, "%d\t%s\t%.2f\t%s\n", i+1, item.symbol, item.direction, judgment)
			}
			w.Flush()
			return nil
		},
	}

	// run サブコマンド
	var cmdRun = &cobra.Command{
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
				// 既にストップ手仕舞い対象なら戦略シグナルはスキップ
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

	cmdRun.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmdRun.Flags().BoolVar(&yesFlag, "yes", false, "本番発注時の確認プロンプトをスキップする")

	// sync サブコマンド
	var cmdSync = &cobra.Command{
		Use:   "sync",
		Short: "ユニバース銘柄の最新日足を J-Quants から同期する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			runID := fmt.Sprintf("wbjp-sync-%d", time.Now().Unix())
			logger, _ := logging.NewLogger("wbjp", string(appSettings.Env), runID, "sync", appSettings.LogDir)
			defer logger.Close()

			barStore := data.NewBarStore(appSettings.BarsDir())
			fredClient := data.NewFREDProvider(15 * time.Second)

			var jqClient *data.JQuantsClient
			if apiKey, err := credentials.LoadAPIKey("WBJP_JQUANTS_API_KEY", appSettings.DotenvMap); err == nil && apiKey != "" {
				jqClient = data.NewJQuantsClient(apiKey)
			}

			fmt.Printf("ユニバース銘柄の日足を同期中（全 %d 銘柄）...\n", len(setCfg.Universe.Symbols))
			for _, sym := range setCfg.Universe.Symbols {
				if err := data.SyncSymbolBars(sym, barStore, jqClient, fredClient, logger); err != nil {
					fmt.Printf("[エラー] %s: %v\n", sym, err)
				} else {
					fmt.Printf("[完了] %s\n", sym)
				}
			}
			return nil
		},
	}

	// backtest サブコマンド
	var cmdBacktest = &cobra.Command{
		Use:   "backtest",
		Short: "過去データを用いてスイング戦略のバックテストを実行する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}
			stratCfg, err := wbjpcfg.LoadStrategiesConfig(configDirFlag)
			if err != nil {
				return err
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			allBars := make(map[string][]domain.Bar)
			for _, sym := range setCfg.Universe.Symbols {
				bars, err := barStore.Read(sym, "", "")
				if err == nil && len(bars) > 0 {
					allBars[sym] = bars
				}
			}

			if len(allBars) == 0 {
				return fmt.Errorf("バックテスト用の足データがありません。先に 'wbjp sync' を実行してください")
			}

			strats, weights := buildStrategies(stratCfg)
			combineFunc := strategy.GetCombinerByName(stratCfg.Combiner)

			stats, err := engine.RunBacktest(setCfg, stratCfg, strats, weights, combineFunc, allBars, decimal.NewFromInt(1000000))
			if err != nil {
				return err
			}

			fmt.Println("=== スイング売買 バックテスト結果 ===")
			fmt.Printf("初期資金: %s 円\n", stats.InitialEquity)
			fmt.Printf("最終資産: %s 円\n", stats.FinalEquity.Round(0))
			fmt.Printf("総リターン: %.2f%%\n", stats.TotalReturn.Mul(decimal.NewFromInt(100)).InexactFloat64())
			fmt.Printf("最大ドローダウン: %.2f%%\n", stats.MaxDrawdown.Mul(decimal.NewFromInt(100)).InexactFloat64())
			fmt.Printf("総約定数: %d 回\n", stats.TotalFills)
			return nil
		},
	}

	rootCmd.AddCommand(cmdAccount)
	rootCmd.AddCommand(cmdStops)
	rootCmd.AddCommand(cmdScreen)
	rootCmd.AddCommand(cmdSync)
	rootCmd.AddCommand(cmdBacktest)
	rootCmd.AddCommand(cmdRun)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
