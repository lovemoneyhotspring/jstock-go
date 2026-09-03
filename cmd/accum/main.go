package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/simulate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/domain"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/settings"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func main() {
	appSettings := settings.LoadAppSettings()

	var rootCmd = &cobra.Command{
		Use:   "accum",
		Short: "日本株・指数の積立投資システム（売却・損切りはせず買い増しのみ）",
	}

	var liveFlag bool
	var yesFlag bool
	var configDirFlag string

	rootCmd.PersistentFlags().StringVar(&configDirFlag, "config-dir", appSettings.ConfigDir, "設定ディレクトリ")

	// strategies サブコマンド
	var cmdStrategies = &cobra.Command{
		Use:   "strategies",
		Short: "使える戦略（タクティクス）の一覧",
		Run: func(cmd *cobra.Command, args []string) {
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "名前\t説明")
			fmt.Fprintln(w, "constant\t定額。常に1倍。純粋なドル平均法")
			fmt.Fprintln(w, "bear_stack\t完全下降配列（終値 < MA20 < MA50 < MA200）で増額")
			fmt.Fprintln(w, "stack_ladder\t弱気スコア（0〜6）に応じて段階的に増額")
			fmt.Fprintln(w, "drawdown_ladder\t過去最高値からの下落率に応じて段階的に増額")
			w.Flush()
		},
	}

	// list サブコマンド
	var cmdList = &cobra.Command{
		Use:   "list",
		Short: "設定されている戦略と銘柄の対応表",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\t戦略\t基本予算\t対象銘柄\t有効")
			for _, t := range cfg.Tactics {
				en := "○"
				if !t.IsEnabled() {
					en = "×"
				}
				fmt.Fprintf(w, "%s\t%s\t%s円\t%v\t%s\n", t.ID, t.Tactic, t.MonthlyBudget, t.Symbols, en)
			}
			w.Flush()
			return nil
		},
	}

	// sync サブコマンド
	var cmdSync = &cobra.Command{
		Use:   "sync",
		Short: "設定銘柄（日本株および判定用指数）の最新日足を同期する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}

			runID := fmt.Sprintf("accum-sync-%d", time.Now().Unix())
			logger, _ := logging.NewLogger("accum", string(appSettings.Env), runID, "sync", appSettings.LogDir)
			defer logger.Close()

			barStore := data.NewBarStore(appSettings.BarsDir())
			fredClient := data.NewFREDProvider(15 * time.Second)

			var jqClient *data.JQuantsClient
			if apiKey, err := credentials.LoadAPIKey("WBJP_JQUANTS_API_KEY", appSettings.DotenvMap); err == nil && apiKey != "" {
				jqClient = data.NewJQuantsClient(apiKey)
			}

			// 同期対象銘柄を収集
			targets := make(map[string]struct{})
			for _, t := range cfg.Tactics {
				if !t.IsEnabled() {
					continue
				}
				for _, s := range t.Symbols {
					targets[s] = struct{}{}
				}
				if t.SignalSymbol != "" {
					targets[t.SignalSymbol] = struct{}{}
				}
			}

			fmt.Printf("日足データを同期中（全 %d 銘柄）...\n", len(targets))
			for sym := range targets {
				if err := data.SyncSymbolBars(sym, barStore, jqClient, fredClient, logger); err != nil {
					fmt.Printf("[エラー] %s: %v\n", sym, err)
				} else {
					fmt.Printf("[完了] %s\n", sym)
				}
			}
			return nil
		},
	}

	// plan サブコマンド
	var cmdPlan = &cobra.Command{
		Use:   "plan",
		Short: "直近の投下計画を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "日付\t銘柄\t終値\t倍率\t基本\t増額\t投下額\t理由")

			for _, entry := range cfg.Tactics {
				if !entry.IsEnabled() {
					continue
				}
				var tactic tactics.Tactic = &tactics.Constant{}
				switch entry.Tactic {
				case "bear_stack":
					tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
				case "stack_ladder":
					tactic = tactics.NewStackLadder(nil, entry.Fast, entry.Mid, entry.Slow)
				case "drawdown_ladder":
					tactic = tactics.NewDrawdownLadder(nil, nil, true, 200)
				}

				var signalBars []domain.Bar
				if entry.SignalSymbol != "" {
					signalBars, _ = barStore.Read(entry.SignalSymbol, "", "")
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, "", "")
					if err != nil || len(bars) == 0 {
						continue
					}
					p, err := plan.BuildPlanWithSignal(bars, signalBars, false, tactic, entry.MonthlyBudget)
					if err != nil || len(p.Rows) == 0 {
						continue
					}
					// 直近5日分を表示
					start := len(p.Rows) - 5
					if start < 0 {
						start = 0
					}
					for i := start; i < len(p.Rows); i++ {
						r := p.Rows[i]
						fmt.Fprintf(w, "%s\t%s\t%s\t%.1f\t%s\t%s\t%s\t%s\n",
							r.Date, sym, r.Close, r.Multiplier, r.Base, r.Extra, r.Amount, r.Reason)
					}
				}
			}
			w.Flush()
			return nil
		},
	}

	// orders サブコマンド
	var cmdOrders = &cobra.Command{
		Use:   "orders",
		Short: "積立の発注台帳を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			led, err := ledger.OpenLedger(appSettings.AccumDBPath())
			if err != nil {
				return err
			}
			defer led.Close()

			orders, err := led.Recent(20)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "日時\t銘柄\t数量\t約定数\t状態\t対象月\t注文ID")
			for _, o := range orders {
				pm := "-"
				if o.PlanMonth != nil {
					pm = *o.PlanMonth
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					clock.FmtISO(o.PlacedAt, clock.Tokyo), o.Symbol, o.Quantity, o.FilledQuantity, o.Status, pm, o.ClientOrderID)
			}
			w.Flush()
			return nil
		},
	}

	// compare サブコマンド
	var cmdCompare = &cobra.Command{
		Use:   "compare [symbol]",
		Short: "1銘柄に対して登録された全戦略を並べてシミュレーション比較する",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			symbol := args[0]
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())
			bars, err := barStore.Read(symbol, "", "")
			if err != nil || len(bars) == 0 {
				return fmt.Errorf("%s の足データがありません。先に 'accum sync' を実行してください", symbol)
			}

			availableTactics := []tactics.Tactic{
				&tactics.Constant{},
				tactics.NewBearStack(4.0, 20, 50, 200),
				tactics.NewBearStack(2.0, 20, 50, 200),
				tactics.NewStackLadder(nil, 20, 50, 200),
				tactics.NewDrawdownLadder(nil, nil, true, 200),
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "=== %s 戦略比較 ===\n", symbol)
			fmt.Fprintln(w, "戦略\t資金倍率\t平均単価\t対照比\t効率\t期末評価額")

			for _, tac := range availableTactics {
				if len(bars) < tac.WarmupBars() {
					continue
				}
				p, err := plan.BuildPlan(bars, tac, cfg.MonthlyBudget)
				if err != nil || len(p.Rows) == 0 {
					continue
				}

				res, err := simulate.Simulate(bars, p, cfg.MonthlyBudget)
				if err != nil {
					continue
				}

				extra := res.CapitalMultiple - 1.0
				efficiency := "—"
				if extra > 0.001 {
					efficiency = fmt.Sprintf("%+.1f%%", (res.CostEdge/extra)*100)
				}

				fmt.Fprintf(w, "%s\t%.2f倍\t%.2f円\t%+.2f%%\t%s\t%.0f円\n",
					tac.Describe(), res.CapitalMultiple, res.AverageCost, res.CostEdge*100, efficiency, res.TerminalValue)
			}
			w.Flush()
			return nil
		},
	}

	// backup サブコマンド
	var cmdBackup = &cobra.Command{
		Use:   "backup [destination]",
		Short: "積立台帳データベース（SQLite）を別ファイルに複製する",
		RunE: func(cmd *cobra.Command, args []string) error {
			dest := fmt.Sprintf("%s.backup-%s", appSettings.AccumDBPath(), time.Now().Format("20060102-150405"))
			if len(args) > 0 {
				dest = args[0]
			}
			srcData, err := os.ReadFile(appSettings.AccumDBPath())
			if err != nil {
				return fmt.Errorf("DB読み込み失敗: %w", err)
			}
			if err := os.WriteFile(dest, srcData, 0644); err != nil {
				return fmt.Errorf("バックアップ作成失敗: %w", err)
			}
			fmt.Printf("積立台帳をバックアップしました: %s\n", dest)
			return nil
		},
	}

	// run サブコマンド
	var cmdRun = &cobra.Command{
		Use:   "run",
		Short: "本日の積立を実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}

			// 口座・発注方針の表示
			canLive, reason := appSettings.CanExecuteLive(liveFlag, cfg.KillSwitch)
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

			// ロガーの初期化
			runID := fmt.Sprintf("accum-%d", time.Now().Unix())
			logger, err := logging.NewLogger("accum", string(appSettings.Env), runID, "run", appSettings.LogDir)
			if err != nil {
				return err
			}
			defer logger.Close()

			// 台帳とストア
			led, err := ledger.OpenLedger(appSettings.AccumDBPath())
			if err != nil {
				return err
			}
			defer led.Close()

			barStore := data.NewBarStore(appSettings.BarsDir())

			// ブローカーの初期化
			var b broker.Broker
			if cfg.Execution.Broker == "paper" || !canLive {
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

			return execute.RunAccumulation(cfg, b, barStore, led, logger, canLive)
		},
	}

	cmdRun.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	// backtest サブコマンド
	var cmdBacktest = &cobra.Command{
		Use:   "backtest",
		Short: "登録された積立戦略の過去検証を実行する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}
			barStore := data.NewBarStore(appSettings.BarsDir())

			for _, entry := range cfg.Tactics {
				if !entry.IsEnabled() {
					continue
				}
				var tactic tactics.Tactic = &tactics.Constant{}
				switch entry.Tactic {
				case "bear_stack":
					tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
				case "stack_ladder":
					tactic = tactics.NewStackLadder(nil, entry.Fast, entry.Mid, entry.Slow)
				case "drawdown_ladder":
					tactic = tactics.NewDrawdownLadder(nil, nil, true, 200)
				}

				var signalBars []domain.Bar
				if entry.SignalSymbol != "" {
					signalBars, _ = barStore.Read(entry.SignalSymbol, "", "")
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, "", "")
					if err != nil || len(bars) == 0 {
						continue
					}
					p, err := plan.BuildPlanWithSignal(bars, signalBars, false, tactic, entry.MonthlyBudget)
					if err != nil || len(p.Rows) == 0 {
						continue
					}

					res, err := simulate.Simulate(bars, p, entry.MonthlyBudget)
					if err != nil {
						fmt.Printf("[%s] 検証失敗: %v\n", sym, err)
						continue
					}

					fmt.Printf("=== 積立バックテスト: %s (%s) ===\n", entry.ID, sym)
					fmt.Printf("期間: %s 〜 %s\n", res.StartDate, res.EndDate)
					fmt.Printf("総投入額: %s 円 (基本予算の %.2f 倍)\n", res.Contributed, res.CapitalMultiple)
					fmt.Printf("平均取得単価: %.2f 円 (対照群比: %+.2f%%)\n", res.AverageCost, res.CostEdge*100)
					fmt.Printf("期末評価額: %.0f 円 (総リターン: %+.2f%%)\n", res.TerminalValue, res.TotalReturn*100)
					fmt.Printf("増額発動日数: %d 日\n\n", res.BoostedDays)
				}
			}
			return nil
		},
	}

	rootCmd.AddCommand(cmdStrategies)
	rootCmd.AddCommand(cmdList)
	rootCmd.AddCommand(cmdSync)
	rootCmd.AddCommand(cmdPlan)
	rootCmd.AddCommand(cmdOrders)
	rootCmd.AddCommand(cmdCompare)
	rootCmd.AddCommand(cmdBackup)
	rootCmd.AddCommand(cmdBacktest)
	rootCmd.AddCommand(cmdRun)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
	_ = filepath.Join
}
