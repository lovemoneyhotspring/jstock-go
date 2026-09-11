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
			"相手のサーバに負担をかけないよう 1 か月ごとに間隔を空ける。",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			months, err := monthRange(from, to)
			if err != nil {
				return err
			}
			store, err := rate.OpenStore(dbPathFlag)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			done, err := store.ImportedMonths(ctx)
			if err != nil {
				return err
			}

			var fetched, skipped, rows int
			for _, ym := range months {
				if done[ym] && !force {
					skipped++
					continue
				}
				if fetched > 0 {
					time.Sleep(interval)
				}
				html, err := rate.FetchWithRetry(ctx, rate.TradersURL+ym, 5, 30*time.Second)
				if err != nil {
					return fmt.Errorf("%s: %w", ym, err)
				}
				entries, err := rate.ParseTraders(html, ym)
				if err != nil {
					// 古い月は行が無いこともある。そこで止めずに記録して進む
					fmt.Printf("%s  取れず: %v\n", ym, err)
					fetched++
					continue
				}
				added, err := store.SaveTraders(ctx, ym, entries)
				if err != nil {
					return err
				}
				fmt.Printf("%s  %4d 行（新規 %4d）\n", ym, len(entries), added)
				fetched++
				rows += len(entries)
			}
			fmt.Printf("取り込み %d か月（既済 %d か月をとばした）/ 合計 %d 行\n", fetched, skipped, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "200501", "開始年月（YYYYMM）")
	cmd.Flags().StringVar(&to, "to", time.Now().Format("200601"), "終了年月（YYYYMM）")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "1 か月ごとに空ける間隔（短すぎると 429 を返される）")
	cmd.Flags().BoolVar(&force, "force", false, "既に取り込んだ月も取り直す")
	return cmd
}

// monthRange は YYYYMM の範囲を並べる。
func monthRange(from, to string) ([]string, error) {
	parse := func(s string) (time.Time, error) {
		t, err := time.Parse("200601", s)
		if err != nil {
			return time.Time{}, fmt.Errorf("年月は YYYYMM で渡してください: %q", s)
		}
		return t, nil
	}
	start, err := parse(from)
	if err != nil {
		return nil, err
	}
	end, err := parse(to)
	if err != nil {
		return nil, err
	}
	if end.Before(start) {
		return nil, fmt.Errorf("終了 %s が開始 %s より前です", to, from)
	}
	var months []string
	for m := start; !m.After(end); m = m.AddDate(0, 1, 0) {
		months = append(months, m.Format("200601"))
	}
	return months, nil
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
