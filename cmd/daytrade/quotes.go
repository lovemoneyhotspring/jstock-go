package main

import (
	"fmt"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newQuotesCmd() *cobra.Command {
	var sourceFlag, quoteFileFlag string
	cmd := &cobra.Command{
		Use:   "quotes SYMBOL...",
		Short: "気配の取得元の疎通を確かめる（寄付の判断に使える鮮度かはここで見る）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			found, err := fetchQuotes(cfg, args, sourceFlag, quoteFileFlag)
			if err != nil {
				return err
			}
			now := clock.NowUTC()
			zone := clock.MustZone(appSettings.Timezone)
			for _, symbol := range args {
				q, ok := found[symbol]
				if !ok {
					fmt.Printf("  %s: 取れませんでした\n", symbol)
					continue
				}
				flag := ""
				if q.Delayed {
					flag = "（遅延）"
				}
				fmt.Printf("  %s: %s 円  %s  %d 秒前 %s%s\n",
					symbol, yen(q.Price), clock.Fmt(q.At, zone, true),
					int(now.Sub(q.At).Seconds()), q.Source, flag)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceFlag, "source", "", "tachibana / csv")
	cmd.Flags().StringVar(&quoteFileFlag, "quote-file", "", "csv のときのファイル")
	return cmd
}
