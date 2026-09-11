package main

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/lovemoneyhotspring/jstock-go/pkg/rate"
	"github.com/spf13/cobra"
)

func newLatestCmd() *cobra.Command {
	var limit int
	var firm string
	var actions string

	cmd := &cobra.Command{
		Use:   "latest",
		Short: "最近記録した行を初出時刻の順に見る",
		RunE: func(cmd *cobra.Command, args []string) error {
			where := []string{"1=1"}
			var args2 []any
			if firm != "" {
				where = append(where, "firm = ?")
				args2 = append(args2, firm)
			}
			if actions != "" {
				var ph []string
				for _, a := range strings.Split(actions, ",") {
					ph = append(ph, "?")
					args2 = append(args2, strings.TrimSpace(a))
				}
				where = append(where, "action IN ("+strings.Join(ph, ",")+")")
			}
			args2 = append(args2, limit)
			return withStore(func(db *sql.DB) error {
				rows, err := db.Query(`
SELECT first_seen_at, pub_date, code, name, firm, rating, target
FROM ratings WHERE `+strings.Join(where, " AND ")+`
ORDER BY first_seen_at DESC, code LIMIT ?`, args2...)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "初出\t掲載日\tコード\t銘柄\t証券会社\tレーティング\t目標株価")
				for rows.Next() {
					var seen, pub, code, name, f, r, t string
					if err := rows.Scan(&seen, &pub, &code, &name, &f, &r, &t); err != nil {
						return err
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", trimTS(seen), pub, code, name, f, r, t)
				}
				if err := rows.Err(); err != nil {
					return err
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 40, "表示する行数")
	cmd.Flags().StringVar(&firm, "firm", "", "証券会社で絞る")
	cmd.Flags().StringVar(&actions, "action", "", "new,up,down,keep で絞る（カンマ区切り）")
	return cmd
}

func newTimingCmd() *cobra.Command {
	var days int
	var maxLag float64
	cmd := &cobra.Command{
		Use:   "timing",
		Short: "証券会社ごとに、掲載日の何時ごろ初めて見えたかを集計する",
		Long: "初出時刻は「取りに行った間隔」までの粗さしか無い。\n" +
			"表の時刻は掲載日 0 時からの経過なので、前夜〜早朝に載ったぶんは 24 時超えで出る。\n" +
			"--max-lag より遅く初めて見えた行は、溜め始める前から載っていた行（初回の取り込み）と\n" +
			"みなして外す。外さないと、過去 1 か月ぶんが全部「取り込んだ時刻に出た」ことになってしまう。",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(db *sql.DB) error {
				rows, err := db.Query(`
SELECT firm,
       COUNT(*) AS n,
       COUNT(DISTINCT pub_date) AS days,
       MIN(hh) AS earliest, AVG(hh) AS mean, MAX(hh) AS latest
FROM (
  SELECT firm, pub_date,
         (julianday(first_seen_at) - julianday(pub_date || 'T00:00:00+09:00')) * 24.0 AS hh
  FROM ratings
  WHERE pub_date >= date('now', '-' || ? || ' day')
    -- 溜め始めた 1 回目は 1 か月ぶんがまとめて入るので、その回の行は時刻を持たない
    AND first_seen_at <> (SELECT MIN(fetched_at) FROM fetches)
)
WHERE hh <= ?
GROUP BY firm ORDER BY mean`, days, maxLag)
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "証券会社\t件数\t日数\t最早\t平均\t最遅")
				for rows.Next() {
					var f string
					var n, d int
					var lo, mean, hi float64
					if err := rows.Scan(&f, &n, &d, &lo, &mean, &hi); err != nil {
						return err
					}
					fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\t%s\n", f, n, d, hhmm(lo), hhmm(mean), hhmm(hi))
				}
				if err := rows.Err(); err != nil {
					return err
				}
				return w.Flush()
			})
		},
	}
	cmd.Flags().IntVar(&days, "days", 90, "さかのぼる日数（掲載日で数える）")
	cmd.Flags().Float64Var(&maxLag, "max-lag", 36, "掲載日 0 時から何時間以内に初めて見えた行までを数えるか")
	return cmd
}

func newQueryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query <SQL>",
		Short: "記録簿に直接 SQL を投げる",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(func(db *sql.DB) error {
				rows, err := db.Query(args[0])
				if err != nil {
					return err
				}
				defer func() { _ = rows.Close() }()
				cols, err := rows.Columns()
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, strings.Join(cols, "\t"))
				for rows.Next() {
					vals := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					if err := rows.Scan(ptrs...); err != nil {
						return err
					}
					cells := make([]string, len(cols))
					for i, v := range vals {
						if b, ok := v.([]byte); ok {
							v = string(b)
						}
						cells[i] = fmt.Sprint(v)
					}
					fmt.Fprintln(w, strings.Join(cells, "\t"))
				}
				if err := rows.Err(); err != nil {
					return err
				}
				return w.Flush()
			})
		},
	}
}

func withStore(fn func(*sql.DB) error) error {
	store, err := rate.OpenStore(dbPathFlag)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return fn(store.DB())
}

// hhmm は掲載日 0 時からの経過時間を HH:MM で見せる（日またぎは 24 時超えのまま出す）。
func hhmm(hours float64) string {
	total := int(hours*60 + 0.5)
	sign := ""
	if total < 0 {
		sign, total = "-", -total
	}
	return fmt.Sprintf("%s%02d:%02d", sign, total/60, total%60)
}

func trimTS(ts string) string {
	if len(ts) >= 16 {
		return strings.Replace(ts[:16], "T", " ", 1)
	}
	return ts
}
