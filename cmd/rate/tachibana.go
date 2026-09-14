package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var days int
	var recent int
	var force bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "立花証券のニュースからレーティングの動きを取り込む（過去 90 日まで）",
		Long: "【株価レーティング】【目標株価】の引き上げ・引き下げ・新規設定は、前営業日ぶんが\n" +
			"翌営業日の 07:10 に配信される。取り込みでは配信日（知れた日）を feed_date として持つので、\n" +
			"検証では feed_date 以降の日にだけ使えばルックアヘッドにならない。\n" +
			"07:10 より前に回すとその日は 0 件で済みになるので、直近の数日は済みでも取り直す（--recent）。\n" +
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

			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			res, err := rate.SyncNews(ctx, store, b, days, recent, force)
			for _, d := range res.Days {
				if d.Events > 0 {
					fmt.Printf("%s  ニュース %3d 件 / レーティングの動き %3d 件\n", d.Day, d.News, d.Events)
				}
			}
			fmt.Printf("取り込み %d 日（既済 %d 日をとばした）/ 動き 合計 %d 件\n", res.Imported, res.Skipped, res.Events)
			return err
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "さかのぼる日数（API の上限は 90）")
	cmd.Flags().IntVar(&recent, "recent", 3, "済みでも取り直す直近の日数（07:10 の配信より前に回した日を拾うため）")
	cmd.Flags().BoolVar(&force, "force", false, "既に取り込んだ日も取り直す")
	return cmd
}
