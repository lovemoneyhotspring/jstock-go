package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newFetchCmd() *cobra.Command {
	var fromFile string
	var quiet bool

	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "一覧ページを取って、初めて見た行を記録簿に足す",
		Long: "cron から数分おきに呼ぶ。既にある行は初出時刻を変えず、最後に見た時刻だけ進める。\n" +
			"取れなかった回も fetches に残すので、「取れなかった時間帯」と「載っていなかった時間帯」を混ぜずに済む。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// 掲載時刻を見るのが目的なので、記録の時刻は常に日本時間で持つ
			now := clock.NowJST()

			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			rows, added, err := rate.FetchAndSave(ctx, store, now, func(ctx context.Context) (string, error) {
				return readSource(ctx, fromFile)
			})
			if err != nil {
				return err
			}
			if !quiet {
				fmt.Printf("%s  ページ %d 行 / 新規 %d 行\n", now.Format("2006-01-02 15:04:05"), rows, added)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "URL の代わりに手元の HTML を読む（動作確認用）")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "新規が無くても何も出さない")
	return cmd
}

func readSource(ctx context.Context, fromFile string) (string, error) {
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		return string(b), err
	}
	return rate.Fetch(ctx, urlFlag)
}
