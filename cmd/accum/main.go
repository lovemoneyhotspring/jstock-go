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
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/tactics"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
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
				if entry.Tactic == "bear_stack" {
					tactic = tactics.NewBearStack(entry.Multiplier, entry.Fast, entry.Mid, entry.Slow)
				}

				for _, sym := range entry.Symbols {
					bars, err := barStore.Read(sym, "", "")
					if err != nil || len(bars) == 0 {
						continue
					}
					p, err := plan.BuildPlan(bars, tactic, entry.MonthlyBudget)
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
	cmdRun.Flags().BoolVar(&yesFlag, "yes", false, "本番発注時の確認プロンプトをスキップする")

	rootCmd.AddCommand(cmdStrategies)
	rootCmd.AddCommand(cmdList)
	rootCmd.AddCommand(cmdPlan)
	rootCmd.AddCommand(cmdOrders)
	rootCmd.AddCommand(cmdRun)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
	_ = filepath.Join
}
