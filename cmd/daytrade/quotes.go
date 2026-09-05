package main

import (
	"encoding/json"
	"fmt"

	dtquotes "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/quotes"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newQuotesCmd() *cobra.Command {
	var sourceFlag, quoteFileFlag, columnsFlag string
	var rawFlag bool
	cmd := &cobra.Command{
		Use:   "quotes SYMBOL...",
		Short: "気配の取得元の疎通を確かめる（寄付の判断に使える鮮度かはここで見る）",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if rawFlag {
				return showRawQuotes(args, columnsFlag)
			}
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
	cmd.Flags().BoolVar(&rawFlag, "raw", false, "時価問合の応答をそのまま JSON で出す（立花のみ。板の列名の確認用）")
	cmd.Flags().StringVar(&columnsFlag, "columns", "",
		"--raw で取りに行く列（sTargetColumn。既定は始値・現在値・現在値時刻・前日終値）")
	return cmd
}

// showRawQuotes は時価問合の応答を解釈せずに出す。
//
// 板の列（気配値・気配数量・特別気配）を記録に足す前に、「その列名で何が返るか」を
// 実機で確かめるための口（docs/OPENING_DATA.md「実機で確かめること」）。仕様書に
// 無い列を指定しても、空で返るのか誤りになるのかがそのまま見える。
func showRawQuotes(symbols []string, columns string) error {
	source, err := dtquotes.Connect(appSettings.Env, appSettings.DotenvMap, appSettings.StateDir)
	if err != nil {
		return err
	}
	rows, err := source.Broker.MarketPricesRaw(symbols, columns)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("  応答に行がありません（銘柄コードか列の指定を確かめてください）")
		return nil
	}
	out, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
