package main

import (
	"errors"
	"fmt"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

// 逆指値の実機検証（docs/BROKER_VERIFY.md の手順 5）。発注・照会・訂正・取消の 4 つは
// 実際に 1 件置かないと項目名が確かめられない。保有している現物に、発火しない水準の
// 売り逆指値を置いて、最後に必ず取消す。
func newVerifyStopCmd() *cobra.Command {
	var symbol string
	var units int
	var dropPct float64
	var liveFlag, yesFlag bool

	cmd := &cobra.Command{
		Use:   "verify-stop",
		Short: "逆指値経路の実機検証: 売り逆指値を置いて、照会・訂正・取消を通す",
		Long: "逆指値の実機検証（docs/BROKER_VERIFY.md 手順 5）。**保有している現物**に対して\n" +
			"売りの逆指値を置き、照会（sOrderGyakusasi* / sOrderTriggerType）・訂正\n" +
			"（CLMKabuCorrectOrder）・取消が通るかを順に見る。\n\n" +
			"条件価格は現在値の −--drop-pct%（既定 3%）で、発火しない水準に置く。\n" +
			"最後に必ず取消す。新規の買いは出さないのでお金は使わない。",
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runVerifyStop(symbol, units, dropPct, liveFlag, yesFlag)
			// 保有不足は柵が働いただけで異常ではない（verify-order の上限超えと同じ）
			var noPos *execute.ErrNoPosition
			if errors.As(err, &noPos) {
				return err
			}
			return run.Crash("逆指値経路の検証", "accum.crash", err)
		},
	}
	cmd.Flags().StringVar(&symbol, "symbol", "", "銘柄コード（保有していること。例 563A）")
	cmd.Flags().IntVar(&units, "units", 1, "売買単位の何倍を売る逆指値にするか")
	cmd.Flags().Float64Var(&dropPct, "drop-pct", 3, "条件価格を現在値から何 % 下に置くか")
	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際に発注する（無ければ何を送るかだけ出す）")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	_ = cmd.MarkFlagRequired("symbol")
	return cmd
}

func runVerifyStop(symbol string, units int, dropPct float64, liveFlag, yesFlag bool) error {
	// この経路は常に検証なので、印は自動で付ける
	run.SetVerify(true)

	cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
	if err != nil {
		return err
	}
	canLive, reason := appSettings.CanExecuteLive(liveFlag, cfg.KillSwitch)
	fmt.Printf("口座: %s  発注: %s（%s）  条件価格: 現在値 −%g%%\n\n",
		appSettings.Env, map[bool]string{true: "する", false: "しない"}[canLive], reason, dropPct)
	if err := cli.ConfirmLive(appSettings, canLive, yesFlag); err != nil {
		return err
	}

	logger, err := newRunLogger("verify-stop")
	if err != nil {
		return err
	}

	var b broker.Broker
	if b, err = run.ConnectBroker(cfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	res, err := execute.VerifyStop(b, logger, execute.VerifyStopOptions{
		Symbol: symbol, Units: units,
		DropPct: decimal.NewFromFloat(dropPct), Live: canLive,
	})
	if res != nil && res.Trigger.IsPositive() {
		fmt.Printf("%s: 売り逆指値 %s 株  条件 %s 円（発火後は成行）\n",
			symbol, res.Request.Quantity, res.Trigger)
	}
	if err != nil {
		return err
	}
	fmt.Println()
	for _, step := range res.Steps {
		mark := "✅"
		if !step.OK {
			mark = "❌"
		}
		fmt.Printf("%s %s: %s\n", mark, step.Name, step.Detail)
	}
	return nil
}
