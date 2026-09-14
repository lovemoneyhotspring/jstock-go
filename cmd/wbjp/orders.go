package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/cli"
	wbjpcfg "github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/execute"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbjp/repo"
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

			b, err := run.ConnectBroker(setCfg.Execution.Broker, appSettings)
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
	var liveFlag bool
	var yesFlag bool

	cmd := &cobra.Command{
		Use:   "cancel [client_order_id]",
		Short: "未約定の注文を取り消す（--live が必要）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientOrderID := args[0]
			setCfg, err := wbjpcfg.LoadSettingsFile(configDirFlag)
			if err != nil {
				return err
			}

			// run と同じ柵（--live・本番環境・確認）。取消は建玉を増やさないので
			// kill_switch では止めない——緊急停止中こそ板の注文を消したい
			canLive, reason := appSettings.CanExecuteLive(liveFlag, false)
			if !canLive {
				return fmt.Errorf("取消を送りません: %s", reason)
			}
			if err := cli.ConfirmLive(appSettings, canLive, yesFlag); err != nil {
				return err
			}

			rep, err := repo.OpenRepo(appSettings.DBPath())
			if err != nil {
				return err
			}
			defer rep.Close()

			b, err := run.ConnectBroker(setCfg.Execution.Broker, appSettings)
			if err != nil {
				return err
			}

			res, err := execute.CancelRecorded(rep, b, clientOrderID)
			if err != nil {
				return err
			}
			fmt.Printf("取消を送信しました: %s\n", clientOrderID)
			switch {
			case !res.Recorded:
				fmt.Println("台帳に記録の無い注文です（台帳は書き換えていません）")
			case res.Deferred:
				fmt.Printf("まだ取消が反映されていません（%s）。次の run の約定同期で台帳に反映します\n", res.Status)
			default:
				fmt.Printf("台帳に記録しました: %s\n", res.Status)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&liveFlag, "live", false, "実際にブローカーへ取消を送る")
	cmd.Flags().BoolVarP(&yesFlag, "yes", "y", false, "本番環境での確認プロンプトをスキップする")
	return cmd
}
