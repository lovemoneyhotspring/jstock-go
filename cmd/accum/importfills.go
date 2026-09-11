package main

import (
	"fmt"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/spf13/cobra"
)

// 手で発注した約定を台帳に取り込む。台帳は「当月いくら投下したか」の記録なので、
// 画面から手で買ったぶんが入らないと plan / run の基準が実態とずれる。
func newImportFillsCmd() *cobra.Command {
	var applyFlag, allFlag bool
	var symbols, orders []string

	cmd := &cobra.Command{
		Use:   "import-fills",
		Short: "手で発注した当月の約定を台帳に取り込む",
		Long: "ブローカーの当月の買い約定のうち、台帳に無いものを台帳に写す。\n\n" +
			"積立の台帳は「当月いくら投下したか」の記録で、plan / run はこれを基準に\n" +
			"「あといくら買うか」を決める。手で買ったぶんを入れないと、二重買付ガードが\n" +
			"止めるか、当月の予算をもう一度買うことになる。\n\n" +
			"既定では積立の設定にある銘柄だけを見る（--all で履歴の買いぜんぶ、\n" +
			"--symbol で銘柄を指定）。--order で注文番号を選べる——同じ銘柄に検証の注文と\n" +
			"本当の買いが混じる日があるので、検証のぶんを投下額として入れないために使う。\n" +
			"取り込みの単位は注文番号なので、2 度叩いても重複しない。\n\n" +
			"**立花証券の注文一覧は当日分しか返らない**ので、手で買った当日に実行すること。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.Crash("約定の取り込み", "accum.crash",
				runImportFills(symbols, orders, allFlag, applyFlag))
		},
	}
	cmd.Flags().StringSliceVar(&symbols, "symbol", nil, "取り込む銘柄コード（例 563A。複数可）")
	cmd.Flags().StringSliceVar(&orders, "order", nil, "取り込む注文番号（例 11014725 または 11014725/20260911。複数可）")
	cmd.Flags().BoolVar(&allFlag, "all", false, "積立の設定に無い銘柄も取り込む")
	cmd.Flags().BoolVar(&applyFlag, "apply", false, "実際に台帳へ書く（無ければ何を入れるかだけ出す）")
	return cmd
}

func runImportFills(symbols, orders []string, allFlag, applyFlag bool) error {
	cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
	if err != nil {
		return err
	}

	// 対象銘柄。--symbol が最優先、次に設定の銘柄、--all なら絞らない
	var wanted []string
	switch {
	case len(symbols) > 0:
		for _, s := range symbols {
			wanted = append(wanted, execute.BrokerSymbol(s))
		}
	case allFlag:
		wanted = nil
	default:
		for _, t := range cfg.Tactics {
			for _, s := range t.Symbols {
				wanted = append(wanted, execute.BrokerSymbol(s))
			}
		}
		for _, b := range cfg.Baskets {
			for _, s := range b.Symbols() {
				wanted = append(wanted, execute.BrokerSymbol(s))
			}
		}
		if len(wanted) == 0 {
			return fmt.Errorf("積立の設定に銘柄がありません。--symbol か --all を指定してください")
		}
	}

	logger, err := newRunLogger("import-fills")
	if err != nil {
		return err
	}
	led, err := ledger.OpenLedger(appSettings.AccumDBPath())
	if err != nil {
		return err
	}
	defer led.Close()

	var b broker.Broker
	if b, err = run.ConnectBroker(cfg.Execution.Broker, appSettings); err != nil {
		return err
	}

	fills, err := execute.ImportFills(led, b, logger, execute.ImportFillsOptions{
		Symbols: wanted, Orders: orders, Apply: applyFlag,
	})
	if err != nil {
		return err
	}
	if len(fills) == 0 {
		fmt.Println("台帳に無い当月の買い約定はありませんでした")
		return nil
	}
	fmt.Printf("=== 台帳に無い当月の買い約定（%d 件）===\n", len(fills))
	total := fills[0].Amount.Sub(fills[0].Amount)
	for _, f := range fills {
		fmt.Printf("  %-6s %8s 株 @ %8s 円 = %10s 円  注文番号 %s\n",
			f.Symbol, f.Quantity, f.AvgFillPrice, f.Amount.Round(0), f.BrokerOrderID)
		total = total.Add(f.Amount)
	}
	fmt.Printf("合計 %s 円\n", total.Round(0))
	if !applyFlag {
		fmt.Println("（--apply が無いので台帳には書いていません）")
		return nil
	}
	fmt.Println("台帳に書きました。`accum orders` で確かめてください")
	return nil
}
