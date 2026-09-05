package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/spf13/cobra"
)

// connectBroker は設定の execution.broker で選んだブローカーに繋ぐ。
func connectBroker() (broker.Broker, error) {
	setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
	if err != nil {
		return nil, err
	}
	return run.ConnectBroker(setCfg.Execution.Broker, appSettings)
}

func newOrderCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "order [client_order_id]",
		Short: "1件の注文の現在の状態を照会する",
		Long: "1件の注文の現在の状態を照会する。\n\n" +
			"取り消し後の確認にも使う。取り消された注文は未約定一覧から消えるが、\n" +
			"ここでは CANCELLED として残る。",
		Args: cobra.ExactArgs(1),
		// 実行や注文が見つからないのは使い方の誤りではないので、使い方は出さない
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			clientOrderID := args[0]
			b, err := connectBroker()
			if err != nil {
				return err
			}

			found, err := b.GetOrder(clientOrderID, nil)
			if err != nil {
				return err
			}
			if found == nil {
				return fmt.Errorf("注文が見つかりません: %s", clientOrderID)
			}

			limit := "成行"
			if found.LimitPrice != nil {
				limit = found.LimitPrice.String()
			}
			fmt.Printf("\n注文 %s (%s)\n", found.ClientOrderID, appSettings.Env)
			fmt.Printf("  銘柄      %s\n", found.Symbol)
			fmt.Printf("  売買/種別 %s / %s\n", found.Side, found.OrderType)
			fmt.Printf("  数量      %s（約定 %s）\n", found.Quantity, found.FilledQuantity)
			fmt.Printf("  指値      %s\n", limit)
			fmt.Printf("  状態      %s\n", found.Status)
			if found.AvgFillPrice != nil {
				fmt.Printf("  約定単価  %s\n", found.AvgFillPrice)
			}
			if found.BrokerOrderID != nil {
				fmt.Printf("  broker_order_id %s\n", *found.BrokerOrderID)
			}
			return nil
		},
	}
}
