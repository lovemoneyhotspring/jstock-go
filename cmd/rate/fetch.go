package main

import (
	"fmt"
	"os"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
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
			now := time.Now().In(jst())

			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			started := time.Now()
			html, err := readSource(cmd, fromFile)
			if err == nil {
				var entries []rate.Entry
				entries, err = rate.Parse(html, now)
				if err == nil {
					added, saveErr := store.Save(ctx, entries, now)
					if saveErr != nil {
						err = saveErr
					} else {
						_ = store.RecordFetch(ctx, now, len(entries), added, "ok", time.Since(started))
						if !quiet {
							fmt.Printf("%s  ページ %d 行 / 新規 %d 行\n", now.Format("2006-01-02 15:04:05"), len(entries), added)
						}
						return nil
					}
				}
			}
			// 失敗も記録に残してから落とす
			_ = store.RecordFetch(ctx, now, 0, 0, err.Error(), time.Since(started))
			return err
		},
	}
	cmd.Flags().StringVar(&fromFile, "from-file", "", "URL の代わりに手元の HTML を読む（動作確認用）")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "新規が無くても何も出さない")
	return cmd
}

func readSource(cmd *cobra.Command, fromFile string) (string, error) {
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		return string(b), err
	}
	return rate.Fetch(cmd.Context(), urlFlag)
}

// jst は記録の時刻帯。掲載時刻を見るのが目的なので、常に日本時間で持つ。
func jst() *time.Location {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		return time.FixedZone("JST", 9*60*60)
	}
	return loc
}
