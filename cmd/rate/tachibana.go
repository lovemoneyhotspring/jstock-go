package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/broker"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/credentials"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var days int
	var force bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "立花証券のニュースからレーティングの動きを取り込む（過去 90 日まで）",
		Long: "【株価レーティング】【目標株価】の引き上げ・引き下げ・新規設定は、前営業日ぶんが\n" +
			"翌営業日の 07:10 に配信される。取り込みでは配信日（知れた日）を feed_date として持つので、\n" +
			"検証では feed_date 以降の日にだけ使えばルックアヘッドにならない。\n" +
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

			done, err := store.ImportedDays(ctx)
			if err != nil {
				return err
			}

			now := time.Now().In(jst())
			imported, skipped, total := 0, 0, 0
			for i := 0; i < days; i++ {
				day := now.AddDate(0, 0, -i)
				key := day.Format("2006-01-02")
				if done[key] && !force {
					skipped++
					continue
				}
				items, err := b.News(day.Format("20060102"))
				if err != nil {
					return fmt.Errorf("%s のニュース取得に失敗: %w", key, err)
				}
				events := rate.EventsFromNews(day, items)
				if err := store.SaveEvents(ctx, key, events, len(items)); err != nil {
					return err
				}
				if len(events) > 0 {
					fmt.Printf("%s  ニュース %3d 件 / レーティングの動き %3d 件\n", key, len(items), len(events))
				}
				imported++
				total += len(events)
			}
			fmt.Printf("取り込み %d 日（既済 %d 日をとばした）/ 動き 合計 %d 件\n", imported, skipped, total)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "さかのぼる日数（API の上限は 90）")
	cmd.Flags().BoolVar(&force, "force", false, "既に取り込んだ日も取り直す")
	return cmd
}
