package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var days int
	var force bool
	var recent int

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "ニュース電文を取り込む（過去 90 日まで）",
		Long: "全ジャンルの見出しと本文をそのまま残す。当日ぶんは 06:00〜23:59 に毎分、\n" +
			"過去ぶんは毎朝 05:40〜05:50 に発信元から取り込まれるので、日次は朝に回す。\n" +
			"直近の数日は追記や訂正が入るため、済みでも取り直す（--recent）。\n" +
			"それより古い日は、0 件（休日）でも ok なら済みとして取り直さない。\n" +
			"API が遡れるのは 90 日まで。それより前は取り返せない。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			creds, err := credentials.LoadTachibanaCredentials(appSettings.Env, appSettings.DotenvMap)
			if err != nil {
				return err
			}
			b, err := broker.NewTachibanaBroker(appSettings.Env, creds, appSettings.StateDir)
			if err != nil {
				return err
			}
			store, err := news.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			res, err := news.Sync(ctx, store, b, days, recent, force)
			for _, d := range res.Days {
				switch {
				case d.Err != nil:
					fmt.Printf("%s  取得に失敗: %v\n", d.Day, d.Err)
				case d.New > 0:
					fmt.Printf("%s  %4d 件（うち初めて %4d 件）\n", d.Day, d.Items, d.New)
				}
			}
			fmt.Printf("取り込み %d 日 / 新しい記事 %d 件（済み %d 日をとばした、失敗 %d 日）\n",
				res.Imported, res.Added, res.Skipped, res.Failed)
			if err != nil {
				return err
			}
			// 日単位の失敗も終了コードで知らせる（台帳には失敗として残り、次回また取りに行く）
			return res.FailureError()
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "さかのぼる日数（API の上限は 90）")
	cmd.Flags().IntVar(&recent, "recent", 3, "済みでも取り直す直近の日数（訂正・追記のため）")
	cmd.Flags().BoolVar(&force, "force", false, "済みの日も全部取り直す")
	return cmd
}
