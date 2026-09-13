package main

import (
	"fmt"
	"time"

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

			done, err := store.DoneDays(ctx)
			if err != nil {
				return err
			}

			now := time.Now().In(jst())
			var imported, skipped, failed, added int
			for i := 0; i < days; i++ {
				day := now.AddDate(0, 0, -i)
				key := day.Format("2006-01-02")
				// 直近 recent 日は訂正が入るので取り直す
				if done[key] && !force && i >= recent {
					skipped++
					continue
				}
				items, err := b.News(day.Format("20060102"))
				if err != nil {
					// 1 日落ちても止めない。落ちた日は台帳に残して次へ
					if rerr := store.RecordFailure(ctx, key, now, err.Error()); rerr != nil {
						return rerr
					}
					fmt.Printf("%s  取得に失敗: %v\n", key, err)
					failed++
					continue
				}
				n, err := store.Save(ctx, key, items, now)
				if err != nil {
					return err
				}
				if n > 0 {
					fmt.Printf("%s  %4d 件（うち初めて %4d 件）\n", key, len(items), n)
				}
				imported++
				added += n
			}
			fmt.Printf("取り込み %d 日 / 新しい記事 %d 件（済み %d 日をとばした、失敗 %d 日）\n",
				imported, added, skipped, failed)
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "さかのぼる日数（API の上限は 90）")
	cmd.Flags().IntVar(&recent, "recent", 3, "済みでも取り直す直近の日数（訂正・追記のため）")
	cmd.Flags().BoolVar(&force, "force", false, "済みの日も全部取り直す")
	return cmd
}

func jst() *time.Location {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.FixedZone("JST", 9*60*60)
	}
	return loc
}
