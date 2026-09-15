package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/spf13/cobra"
)

// newEventsCmd は適時開示の見出しから判定した材料（TOB・MBO など）を並べる。
// daytrade がショートから外す銘柄と同じ判定（news.Classify）を目で確かめるためのもの。
func newEventsCmd() *cobra.Command {
	var from, to, kind string
	var summary bool
	cmd := &cobra.Command{
		Use:   "events",
		Short: "適時開示から TOB・MBO など価格の行き先が決まる材料を判定して並べる",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if to == "" {
				to = time.Now().Format("2006-01-02")
			}
			return withStore(func(s *news.Store) error {
				events, err := s.CorporateEvents(ctx, from, to, time.Now())
				if err != nil {
					return err
				}
				var rows []news.Event
				for _, es := range events {
					for _, e := range es {
						if kind == "" || e.Kind == kind {
							rows = append(rows, e)
						}
					}
				}
				sort.Slice(rows, func(i, j int) bool {
					if rows[i].FeedDate+rows[i].Time != rows[j].FeedDate+rows[j].Time {
						return rows[i].FeedDate+rows[i].Time < rows[j].FeedDate+rows[j].Time
					}
					return rows[i].Code < rows[j].Code
				})
				if summary {
					type agg struct{ n, codes int }
					by := map[string]*agg{}
					seen := map[string]bool{}
					for _, e := range rows {
						a := by[e.Kind]
						if a == nil {
							a = &agg{}
							by[e.Kind] = a
						}
						a.n++
						if !seen[e.Kind+e.Code] {
							seen[e.Kind+e.Code] = true
							a.codes++
						}
					}
					w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
					_, _ = fmt.Fprintln(w, "種類\t外す\t件数\t銘柄")
					for k, a := range by {
						_, _ = fmt.Fprintf(w, "%s\t%v\t%d\t%d\n", k, news.Excludes(k), a.n, a.codes)
					}
					return w.Flush()
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				for _, e := range rows {
					_, _ = fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\n", e.FeedDate, e.Time, e.Code, e.Kind, e.Headline)
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", time.Now().AddDate(0, 0, -120).Format("2006-01-02"), "配信日の始め（YYYY-MM-DD）")
	cmd.Flags().StringVar(&to, "to", "", "配信日の終わり（既定は今日）")
	cmd.Flags().StringVar(&kind, "kind", "", "種類で絞る（tob_target / mbo / squeeze_out / share_exchange / delisting / self_tender / accumulation / press_report）")
	cmd.Flags().BoolVar(&summary, "summary", false, "種類ごとの件数と銘柄数だけ出す")
	return cmd
}
