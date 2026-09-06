package main

import (
	"fmt"

	dtledger "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/ledger"
	dtplan "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/plan"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var dateFlag string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "今日の候補と台帳の注文を表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			day, err := dayOrToday(dateFlag, clock.NowUTC())
			if err != nil {
				return err
			}
			state := "有効"
			if !cfg.Capital.Enabled {
				state = "無効（capital.enabled = false）"
			}
			suffix := ""
			if cfg.Capital.Positions() == 0 {
				suffix = "（資金 0: 買わない）"
			}
			fmt.Printf("%s: %s  資金 %s 円 → N=%d、1 注文 %s 円%s\n",
				cfg.StrategyName(), state, yen(cfg.Capital.MaxCapital),
				cfg.Capital.Positions(), yen(cfg.Capital.BudgetPerOrder()), suffix)

			p, ok, err := dtplan.Load(appSettings.DaytradeDir(), day)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Printf("%s の候補がありません（daytrade plan を実行）\n", day.Format(DateLayout))
			} else {
				printPlan(p, cfg)
			}

			led, err := dtledger.Open(appSettings.DaytradeDBPath())
			if err != nil {
				return err
			}
			defer led.Close()
			orders, err := led.OrdersOn(day, nil)
			if err != nil {
				return err
			}
			if len(orders) == 0 {
				fmt.Println("今日の注文はありません")
				return nil
			}
			fmt.Printf("\n%s の注文\n", day.Format(DateLayout))
			fmt.Printf("  %-25s %-6s %-4s %10s %10s %10s %12s %s\n",
				"時刻", "銘柄", "売買", "株数", "約定", "価格", "約定単価", "状態")
			verifying := 0
			for _, o := range orders {
				// 実機検証の注文は成績に数えないので、一覧でも見分けが付くようにする
				status := o.Status
				if o.Verify {
					status += "（検証）"
					verifying++
				}
				fmt.Printf("  %-25s %-6s %-4s %10s %10s %10s %12s %s\n",
					clock.FmtISO(o.PlacedAt, clock.MustZone(appSettings.Timezone)),
					o.Symbol, string(o.Side), yen(o.Quantity), yen(o.FilledQuantity),
					yenPtr(o.Price), yenPtr(o.AvgFillPrice), status)
			}
			if verifying > 0 {
				fmt.Printf("\n  うち %d 件は実機検証（--broker-verify）。成績の集計と資産曲線のゲートからは外れます\n",
					verifying)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dateFlag, "date", "", "判定日（YYYY-MM-DD、既定は今日）")
	return cmd
}
