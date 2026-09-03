package main

import (
	"fmt"
	"os"
	"time"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/logging"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newRunCmd() *cobra.Command {
	var liveFlag bool
	var yesFlag bool
	var ignoreWindowFlag bool
	var noSyncFlag bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "本日の積立を実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
			if err != nil {
				return err
			}

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
				// 非対話（cron・パイプ）では確認を取れない。黙って通すと
				// 意図しない本番発注になるので、明示的に --yes を求める。
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fmt.Errorf("非対話で本番発注はできません。確認を省くなら --yes を付けてください")
				}
				fmt.Print("本番注文を送信します。続行しますか？ [y/N]: ")
				var input string
				_, _ = fmt.Scanln(&input)
				if input != "y" && input != "Y" {
					fmt.Println("中止しました。")
					return nil
				}
			}

			runID := fmt.Sprintf("accum-%d", time.Now().Unix())
			logger, err := logging.NewLogger("accum", string(appSettings.Env), runID, "run", appSettings.LogDir)
			if err != nil {
				return err
			}
			defer logger.Close()

			// 判断の前に足を更新する。cron の sync とは独立に、この実行が見る
			// 足を自分で最新にしてから倍率を決める（--no-sync で抑止）。
			// 長い履歴は保存済みなので、直近ぶんだけ取り直せば足りる。
			if !noSyncFlag {
				if _, failures := syncAccumBars(cfg, logger, runSyncDays, false, false); failures > 0 {
					logger.Warn("accum.sync_failed",
						fmt.Sprintf("%d 銘柄の足を更新できませんでした（保存済みの足で続けます）", failures))
				}
			}

			barStore := data.NewBarStore(appSettings.BarsDir())
			led, err := ledger.OpenLedger(appSettings.AccumDBPath())
			if err != nil {
				return err
			}
			defer led.Close()

			var b broker.Broker
			if !canLive {
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

			// 判断の履歴（発注に至らなかった日も含む）を残す。accum evaluate の材料。
			hist := accumhist.StoreFor(appSettings)

			return execute.RunAccumulation(cfg, b, barStore, led, logger, hist, canLive, ignoreWindowFlag)
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "発注時間帯の外でも注文を作る")
	cmd.Flags().BoolVar(&noSyncFlag, "no-sync", false, "足の更新をしない")
	return cmd
}
