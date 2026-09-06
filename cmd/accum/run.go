package main

import (
	"fmt"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	accumhist "github.com/lovemoneyhotspring/jstock-go/pkg/accum/history"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/data"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newRunCmd() *cobra.Command {
	var liveFlag bool
	var yesFlag bool
	var ignoreWindowFlag bool
	var noSyncFlag bool
	var brokerVerifyFlag bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "本日の積立を実行する（--live で発注）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.Crash("積立の実行", "accum.crash",
				runAccumulation(liveFlag, yesFlag, ignoreWindowFlag, noSyncFlag, brokerVerifyFlag))
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ発注する")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	cmd.Flags().BoolVar(&ignoreWindowFlag, "ignore-window", false, "発注時間帯の外でも注文を作る")
	cmd.Flags().BoolVar(&noSyncFlag, "no-sync", false, "足の更新をしない")
	cmd.Flags().BoolVar(&brokerVerifyFlag, "broker-verify", false,
		"発注経路の実機検証（docs/BROKER_VERIFY.md）。台帳・ログ・ダイジェストに印を付ける")
	return cmd
}

// runAccumulation は本体。RunE から切り出してあるのは、異常終了を run.Crash で記録・通知するため。
func runAccumulation(liveFlag, yesFlag, ignoreWindowFlag, noSyncFlag, brokerVerifyFlag bool) error {
	// 以降のログの全行とダイジェストに印を付ける（env とは独立）
	run.SetVerify(brokerVerifyFlag)
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

	if err := cli.ConfirmLive(appSettings, canLive, yesFlag); err != nil {
		return err
	}

	logger, err := newRunLogger("run")
	if err != nil {
		return err
	}

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
	led.Verify = brokerVerifyFlag

	// dry-run は常にメモリ上の模型
	var b broker.Broker
	if !canLive {
		b = broker.NewPaperBroker(decimal.Zero, "open")
	} else if b, err = run.ConnectBroker(cfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	// 判断の履歴（発注に至らなかった日も含む）を残す。accum evaluate の材料。
	hist := accumhist.StoreFor(appSettings)

	return execute.RunAccumulation(cfg, b, barStore, led, logger, hist, canLive, ignoreWindowFlag)
}
