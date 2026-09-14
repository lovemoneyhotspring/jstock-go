package main

import (
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
	"github.com/spf13/cobra"
)

func newHistoryCmd() *cobra.Command {
	var from, to string
	var interval time.Duration
	var force bool

	cmd := &cobra.Command{
		Use:   "history",
		Short: "月別の過去一覧を取り込む（2005 年〜）",
		Long: "月ごとのページを順に取る。既に取り込んだ月はとばす（--force で取り直す）。\n" +
			"相手のサーバに負担をかけないよう 1 か月ごとに間隔を空ける。\n" +
			"429・5xx・接続の失敗は 30 秒から倍々に待って取り直し、それでも駄目ならそこで止める\n" +
			"（済んだ月は残るので、再実行すれば続きから）。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			months, err := rate.MonthRange(from, to)
			if err != nil {
				return err
			}
			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			res, err := rate.ImportMonths(ctx, store, months, force, interval, rate.TradersFetcher(5, 30*time.Second),
				func(m rate.MonthResult) {
					if m.ParseErr != nil {
						// 古い月は行が無いこともある。そこで止めずに進む
						fmt.Printf("%s  取れず: %v\n", m.YearMonth, m.ParseErr)
						return
					}
					fmt.Printf("%s  %4d 行（新規 %4d）\n", m.YearMonth, m.Rows, m.Added)
				})
			fmt.Printf("取り込み %d か月（既済 %d か月をとばした）/ 合計 %d 行\n", res.Fetched, res.Skipped, res.Rows)
			return err
		},
	}
	cmd.Flags().StringVar(&from, "from", "200501", "開始年月（YYYYMM）")
	cmd.Flags().StringVar(&to, "to", time.Now().Format("200601"), "終了年月（YYYYMM）")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "1 か月ごとに空ける間隔（短すぎると 429 を返される）")
	cmd.Flags().BoolVar(&force, "force", false, "既に取り込んだ月も取り直す")
	return cmd
}

func newDirectionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "direction",
		Short: "過去一覧の direction（上げ・下げ）を計算し直す",
		Long: "投資判断の序列表を直したときに使う。トレーダーズの表記には「格上げ」「格下げ」の語が\n" +
			"無く、\"Buy→Hold\" のように前後が並ぶだけなので、序列に当てて向きを出している。",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			n, err := store.RecomputeDirections(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Printf("%d 行を計算し直しました\n", n)
			return nil
		},
	}
}
