package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/spf13/cobra"
)

func newOrdersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "orders",
		Short: "未約定の注文一覧を client_order_id 付きで表示する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			b, err := cli.ConnectBroker(setCfg.Execution.Broker, appSettings)
			if err != nil {
				return err
			}

			openOrders, err := b.GetOpenOrders()
			if err != nil {
				return err
			}

			if len(openOrders) == 0 {
				fmt.Printf("未約定注文はありません (%s)\n", appSettings.Env)
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "注文ID\t銘柄\t売買\t種別\t数量\t未約定\t指値\t状態")
			for _, o := range openOrders {
				lp := "成行"
				if o.LimitPrice != nil {
					lp = o.LimitPrice.String()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					o.ClientOrderID, o.Symbol, o.Side, o.OrderType, o.Quantity, o.RemainingQuantity(), lp, o.Status)
			}
			w.Flush()
			return nil
		},
	}
}

func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel [client_order_id]",
		Short: "未約定の注文を取り消す",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientOrderID := args[0]
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			b, err := cli.ConnectBroker(setCfg.Execution.Broker, appSettings)
			if err != nil {
				return err
			}

			if err := b.Cancel(clientOrderID, nil); err != nil {
				return fmt.Errorf("取消に失敗しました: %w", err)
			}

			fmt.Printf("取消を送信しました: %s\n", clientOrderID)
			return nil
		},
	}
}
