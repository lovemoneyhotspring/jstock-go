package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/clock"
	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	var days int
	var only []string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "台帳と取引カレンダーを見て、必要な端点・日付だけ取り込む（冪等）",
		Long: "cron はこれを固定間隔で叩く。台帳に「いつ何を取ったか」が残っているので、\n" +
			"何度実行しても取り直しは訂正の猶予ぶんに限られる。",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --days 省略時は端点ごとの settle_days を使う（負で「未指定」を表す）
			lookback := -1
			if cmd.Flags().Changed("days") {
				lookback = days
			}
			// dry-run は API を叩かないので、キーが無くても計画だけは見せる
			s, err := newSession("sync", !dryRun)
			if err != nil {
				return err
			}
			defer s.close()

			now := clock.NowUTC()
			if dryRun {
				jobs, err := s.ingestor.Plan(now, lookback)
				if err != nil {
					return err
				}
				wanted := map[string]bool{}
				if len(only) > 0 {
					eps, err := resolveEndpoints(only)
					if err != nil {
						return err
					}
					for _, ep := range eps {
						wanted[ep.Path] = true
					}
				}
				var lines [][3]string
				for _, job := range jobs {
					if len(wanted) > 0 && !wanted[job.Endpoint.Path] {
						continue
					}
					// 引数は順序を固定して並べる（map の走査順は不定）
					keys := make([]string, 0, len(job.Params))
					for k := range job.Params {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					params := make([]string, 0, len(keys))
					for _, k := range keys {
						params = append(params, fmt.Sprintf("%s=%s", k, job.Params[k]))
					}
					lines = append(lines, [3]string{job.Endpoint.Path, job.Target, strings.Join(params, ", ")})
				}
				if len(lines) == 0 {
					fmt.Println("やることはありません（すべて最新）")
					return nil
				}
				fmt.Printf("やること（%s）\n", clock.Fmt(now, clock.MustZone(appSettings.Timezone), false))
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "端点\t対象\t引数")
				for _, line := range lines {
					fmt.Fprintf(w, "%s\t%s\t%s\n", line[0], line[1], line[2])
				}
				return w.Flush()
			}

			result, err := s.ingestor.Sync(now, lookback, only)
			if err != nil {
				return err
			}
			printIngests(result.Ingests,
				fmt.Sprintf("取り込み（%s）", clock.Fmt(clock.NowUTC(), clock.MustZone(appSettings.Timezone), false)))
			if printFailures(result.Failures, "次回の sync で再試行されます") {
				os.Exit(1)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&days, "days", 0, "何日遡って「一度も取っていない日」を埋めるか（省略時は訂正の猶予ぶんだけ）")
	cmd.Flags().StringSliceVar(&only, "only", nil, "端点を絞る（名前かパス。複数可）")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "取らずに、やることだけ表示")
	return cmd
}
