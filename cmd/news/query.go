package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/news"
	"github.com/spf13/cobra"
)

func withStore(fn func(*news.Store) error) error {
	store, err := news.OpenStore(dbPathFlag)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return fn(store)
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "どれだけ溜まっているかと、ジャンル別の件数を見る",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return withStore(func(s *news.Store) error {
				days, items, first, last, err := s.Coverage(ctx)
				if err != nil {
					return err
				}
				if days == 0 {
					fmt.Println("まだ 1 日も溜まっていません（news sync）")
					return nil
				}
				fmt.Printf("%d 営業日 / %d 件  %s 〜 %s（1 日あたり %.0f 件）\n",
					days, items, first, last, float64(items)/float64(days))

				rows, err := s.DB().QueryContext(ctx, `
                    SELECT g.genre, COUNT(*) AS n,
                           SUM(CASE WHEN n.time BETWEEN '0900' AND '1530' THEN 1 ELSE 0 END) AS in_session
                    FROM news_genres g JOIN news n
                      ON n.feed_date = g.feed_date AND n.news_id = g.news_id
                    GROUP BY g.genre ORDER BY n DESC LIMIT 25`)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "ジャンル\t件数\t場中")
				for rows.Next() {
					var genre string
					var n, inSession int
					if err := rows.Scan(&genre, &n, &inSession); err != nil {
						return err
					}
					fmt.Fprintf(w, "%s\t%d\t%.0f%%\n", genre, n, float64(inSession)/float64(n)*100)
				}
				if err := rows.Err(); err != nil {
					return err
				}
				return w.Flush()
			})
		},
	}
}

func newListCmd() *cobra.Command {
	var code, genre, day, word string
	var limit int
	var body bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "溜めた記事を銘柄・ジャンル・日付・語で引く",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			where := []string{"1=1"}
			var params []any
			from := "news n"
			if code != "" {
				from += " JOIN news_codes c ON c.feed_date = n.feed_date AND c.news_id = n.news_id"
				where = append(where, "c.code = ?")
				params = append(params, code)
			}
			if genre != "" {
				from += " JOIN news_genres g ON g.feed_date = n.feed_date AND g.news_id = n.news_id"
				where = append(where, "g.genre = ?")
				params = append(params, genre)
			}
			if day != "" {
				where = append(where, "n.feed_date = ?")
				params = append(params, day)
			}
			if word != "" {
				where = append(where, "(n.headline LIKE ? OR n.body LIKE ?)")
				params = append(params, "%"+word+"%", "%"+word+"%")
			}
			params = append(params, limit)

			return withStore(func(s *news.Store) error {
				rows, err := s.DB().QueryContext(ctx, `
                    SELECT n.feed_date, n.time, n.genres, n.codes, n.headline, n.body
                    FROM `+from+` WHERE `+strings.Join(where, " AND ")+`
                    ORDER BY n.feed_date DESC, n.time DESC LIMIT ?`, params...)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				for rows.Next() {
					var d, tm, genres, codes, head, text string
					if err := rows.Scan(&d, &tm, &genres, &codes, &head, &text); err != nil {
						return err
					}
					fmt.Printf("%s %s  GN=%-8s 銘柄=%-20s %s\n", d, tm, genres, codes, head)
					if body && text != "" {
						fmt.Printf("    %s\n", strings.ReplaceAll(text, "\n", "\n    "))
					}
				}
				return rows.Err()
			})
		},
	}
	cmd.Flags().StringVar(&code, "code", "", "銘柄コード")
	cmd.Flags().StringVar(&genre, "genre", "", "ジャンル（62199 = TDnet 適時開示）")
	cmd.Flags().StringVar(&day, "date", "", "配信日（YYYY-MM-DD）")
	cmd.Flags().StringVar(&word, "word", "", "見出しか本文に含まれる語")
	cmd.Flags().IntVar(&limit, "limit", 30, "表示する件数")
	cmd.Flags().BoolVar(&body, "body", false, "本文も出す")
	return cmd
}
