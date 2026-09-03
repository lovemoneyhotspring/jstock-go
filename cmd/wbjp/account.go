package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/shopspring/decimal"
	"github.com/spf13/cobra"
)

func newAccountCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "account",
		Short: "口座残高と保有建玉を確認する",
		RunE: func(cmd *cobra.Command, args []string) error {
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			var b broker.Broker
			if setCfg.Execution.Broker == "paper" {
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

			bal, err := b.GetBalance()
			if err != nil {
				return fmt.Errorf("残高照会エラー: %w", err)
			}

			positions, err := b.GetPositions()
			if err != nil {
				return fmt.Errorf("建玉照会エラー: %w", err)
			}

			fmt.Printf("=== 口座情報 (%s: %s) ===\n", setCfg.Execution.Broker, appSettings.Env)
			fmt.Printf("現金残高: %s 円\n", bal.CashBalance)
			fmt.Printf("買付余力: %s 円\n", bal.BuyingPower)
			fmt.Printf("建玉評価額: %s 円\n\n", bal.MarketValue)

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "銘柄\t数量\t取得単価\t現在値\t評価損益")
			for _, pos := range positions {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					pos.Symbol, pos.Quantity, pos.CostPrice, pos.LastPrice, pos.UnrealizedPnL())
			}
			w.Flush()
			return nil
		},
	}
}
