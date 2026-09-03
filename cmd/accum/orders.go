package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	accumcfg "github.com/lovemoneyhotspring/jstock-go/pkg/accum/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/accum/ledger"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newOrdersCmd() *cobra.Command {
	var limit int
	var check bool

	cmd := &cobra.Command{
		Use:   "orders",
		Short: "送った注文とその約定状況（台帳）を表示する",
		Long: "「発注済み」に数える額は、生きている注文と約定した分だけ。失効・拒否の\n" +
			"未約定分は数えず、次の run で差額として埋め直される。",
		RunE: func(cmd *cobra.Command, args []string) error {
			led, err := ledger.OpenLedger(appSettings.AccumDBPath())
			if err != nil {
				return err
			}
			defer led.Close()

			if check {
				cfg, err := accumcfg.LoadAccumConfig(configDirFlag)
				if err != nil {
					return err
				}
				if err := checkOpenOrders(cfg, led); err != nil {
					return err
				}
			}

			orders, err := led.Recent(limit)
			if err != nil {
				return err
			}
			if len(orders) == 0 {
				fmt.Println("台帳に注文はありません")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "=== 積立の注文（%s） ===\n", appSettings.Env)
			fmt.Fprintln(w, "送信\t銘柄\t月\t投下額\t株数\t約定\t平均価格\t状態\t有効額")
			for _, o := range orders {
				month := "—"
				if o.PlanMonth != nil && len(*o.PlanMonth) >= 7 {
					month = (*o.PlanMonth)[:7]
				}
				amount := "—"
				if o.Amount != nil {
					amount = o.Amount.StringFixed(0)
				}
				avg := "—"
				if o.AvgFillPrice != nil {
					avg = o.AvgFillPrice.StringFixed(2)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					clock.FmtISO(o.PlacedAt, clock.Tokyo), o.Symbol, month, amount,
					o.Quantity, o.FilledQuantity, avg, o.Status, o.EffectiveAmount().StringFixed(0))
			}
			w.Flush()
			fmt.Println("有効額＝「発注済み」に数える額。失効・拒否は約定ぶんだけ。dry-run は 0")
			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "表示件数")
	cmd.Flags().BoolVar(&check, "check", false, "結果が確定していない注文をブローカーに照会して更新する")
	return cmd
}

// checkOpenOrders は未確定の注文をブローカーに照会し、台帳を更新する。
//
// 台帳の「発注済み」の額は発注時の想定額（指値・直近値）で入っている。約定額が
// 分かった時点で株数 × 約定単価に置き換えないと、当月の残りの計算がずれる。
func checkOpenOrders(cfg *accumcfg.AccumConfig, led *ledger.Ledger) error {
	open, err := led.OpenOrders()
	if err != nil {
		return err
	}
	if len(open) == 0 {
		fmt.Println("結果待ちの注文はありません")
		return nil
	}

	b, err := connectBroker(cfg)
	if err != nil {
		return err
	}
	synced, err := execute.SyncOrderStatus(led, b, clock.NowUTC())
	for _, change := range synced.Changes {
		fmt.Println("更新: " + change.Describe())
	}
	// 照会できなかった注文は台帳をそのままにしてある。人が口座を見て
	// 判断する必要があるので、黙って「変化なし」にはしない。
	for _, u := range synced.Unresolved {
		fmt.Println("保留（照会できず）: " + u.Describe())
	}
	if err != nil {
		return err
	}
	if len(synced.Changes) == 0 && len(synced.Unresolved) == 0 {
		fmt.Println("変化のあった注文はありません")
	}
	if len(synced.Unresolved) > 0 {
		return fmt.Errorf("%d 件の注文を照会できませんでした", len(synced.Unresolved))
	}
	return nil
}

// connectBroker は設定の発注先に繋ぐ。本番口座でなければ紙のブローカーで済ませる。
func connectBroker(cfg *accumcfg.AccumConfig) (broker.Broker, error) {
	if cfg.Execution.Broker == "paper" {
		return broker.NewPaperBroker(decimal.Zero, "open"), nil
	}
	creds, err := credentials.LoadTachibanaCredentials(appSettings.Env, appSettings.DotenvMap)
	if err != nil {
		return nil, err
	}
	return broker.NewTachibanaBroker(appSettings.Env, creds, appSettings.StateDir)
}
