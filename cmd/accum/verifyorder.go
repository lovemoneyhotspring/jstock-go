package main

import (
	"errors"
	"fmt"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

// 発注経路の実機検証（docs/BROKER_VERIFY.md）。注文照会の項目名は、実際に 1 件
// 出さないと確かめられない。`accum run` は月初の入金日と増額日にしか注文を作らないので、
// 検証したい日に出す口をここに置く。
func newVerifyOrderCmd() *cobra.Command {
	var symbol string
	var units int
	var maxYen int64
	var liveFlag, yesFlag bool

	cmd := &cobra.Command{
		Use:   "verify-order",
		Short: "発注経路の実機検証: 1 単元だけ買って、照会で拾えるかを確かめる",
		Long: "発注経路の実機検証（docs/BROKER_VERIFY.md）。売買単位 1 単元だけを指値で買い、\n" +
			"直後に照会し直して、約定数量・約定単価・注文状態の項目名が実データで読めるかを見る。\n\n" +
			"買いだけ・現物だけ・1 単元だけ。**見積り金額が --max-yen を超えたら送らない**。\n" +
			"台帳には検証の印が付き、成績の集計から外れる。",
		RunE: func(cmd *cobra.Command, args []string) error {
			err := runVerifyOrder(symbol, units, maxYen, liveFlag, yesFlag)
			// 上限超えは柵が働いただけで異常ではない。Crash に渡すと Discord に
			// alert が飛び、夜間の自己修復と日次レポートが本当の異常として拾う
			var overLimit *execute.ErrOverLimit
			if errors.As(err, &overLimit) {
				return err
			}
			return run.Crash("発注経路の検証", "accum.crash", err)
		},
	}
	cmd.Flags().StringVar(&symbol, "symbol", "", "銘柄コード（例 563A。.T は付けない）")
	cmd.Flags().IntVar(&units, "units", 1, "売買単位の何倍か")
	cmd.Flags().Int64Var(&maxYen, "max-yen", 2000, "見積り金額の上限（円）。超えたら送らない")
	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際に発注する（無ければ何を送るかだけ出す）")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番発注時の確認プロンプトをスキップする")
	_ = cmd.MarkFlagRequired("symbol")
	return cmd
}

func runVerifyOrder(symbol string, units int, maxYen int64, liveFlag, yesFlag bool) error {
	// この経路は常に検証なので、印は自動で付ける（付け忘れると夜間の自己修復と
	// 日次レポートが本当の異常として拾う）
	run.SetVerify(true)

	cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
	if err != nil {
		return err
	}
	canLive, reason := appSettings.CanExecuteLive(liveFlag, cfg.KillSwitch)
	fmt.Printf("口座: %s  発注: %s（%s）  上限 %d 円\n\n",
		appSettings.Env, map[bool]string{true: "する", false: "しない"}[canLive], reason, maxYen)
	if err := cli.ConfirmLive(appSettings, canLive, yesFlag); err != nil {
		return err
	}

	logger, err := newRunLogger("verify-order")
	if err != nil {
		return err
	}
	led, err := ledger.OpenLedger(appSettings.AccumDBPath())
	if err != nil {
		return err
	}
	defer led.Close()
	led.Verify = true

	var b broker.Broker
	if b, err = run.ConnectBroker(cfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	res, err := execute.VerifyOrder(b, led, logger, execute.VerifyOrderOptions{
		Symbol: symbol, Units: units,
		MaxYen: decimal.NewFromInt(maxYen), Live: canLive,
	})
	if res != nil && res.Lot.IsPositive() {
		fmt.Printf("%s: 売買単位 %s 株 × %d 単元 × %s 円 = 見積り %s 円\n",
			symbol, res.Lot, units, res.Price, res.Estimate.Round(0))
	}
	if err != nil {
		return err
	}
	if res.Ack == nil {
		fmt.Println("（--live が無いので送っていません）")
		return nil
	}
	fmt.Printf("発注しました: ID %s\n", res.Ack.ClientOrderID)
	if res.Queried == nil {
		fmt.Println("⚠ 発注直後の照会で拾えませんでした。`accum orders --check` で追ってください")
		return nil
	}
	q := res.Queried
	avg := "—"
	if q.AvgFillPrice != nil {
		avg = q.AvgFillPrice.String()
	}
	fmt.Printf("照会できました: 状態 %s  数量 %s  約定数量 %s  約定単価 %s\n",
		q.Status, q.Quantity, q.FilledQuantity, avg)
	return nil
}
