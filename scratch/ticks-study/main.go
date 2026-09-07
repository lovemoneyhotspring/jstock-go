// 使い捨ての検証プログラム（ティックで、寄り・引けのどの時間帯の情報がトレードを有利にするかを測る）。
//
//	go run ./scratch/ticks-study -extract    パネル（母集団）とティックの特徴量を /tmp/ticks に書く
//	go run ./scratch/ticks-study -analyze    特徴量を読み、シグナル単位の横断で表を出す
//
// 検証ノート: ~/obsidian-vault/20-research/2026-09-jp-gap-ticks.md
package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	dtbacktest "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

const outDir = "/tmp/ticks"
const jqDir = "data/jquants"
const dayLayout = "2006-01-02"

// fillSecs は「9:00 から X 秒後に成行を出した」の建値を取る秒（その秒以降の最初の約定）。
var fillSecs = []int{5, 10, 20, 30, 45, 60, 90, 120, 180, 300, 600, 900, 1200, 1800}

// markSecs は「9:00 から X 秒後の値洗い」（その秒以前の最後の約定）。
var markSecs = []int{5, 10, 20, 30, 60, 120, 300, 600}

// closeMarks は引け側の値洗い時刻（その時刻より前の最後の約定）。
var closeMarks = []string{"15:00", "15:05", "15:10", "15:15", "15:20", "15:25"}

// closeFills は引け側の成行の時刻（その時刻以降の最初の約定）。
var closeFills = []string{"15:00", "15:10", "15:15", "15:20", "15:22", "15:24"}

func tkey(t string) string { return strings.ReplaceAll(t, ":", "") }

func main() {
	var (
		configDir = flag.String("config-dir", "config/daytrade_margin", "")
		since     = flag.String("since", "2024-11-05", "")
		until     = flag.String("until", "2026-09-04", "")
		extract   = flag.Bool("extract", false, "")
		analyze   = flag.Bool("analyze", false, "")
		only      = flag.String("only", "", "analyze で出す表をカンマ区切りで絞る")
	)
	flag.Parse()
	start, _ := time.Parse(dayLayout, *since)
	end, _ := time.Parse(dayLayout, *until)
	cfg, err := dtconfig.Load(*configDir)
	must(err)
	must(os.MkdirAll(outDir, 0o755))

	if *extract {
		arch := archive.NewArchive(jqDir)
		t0 := time.Now()
		panel, err := dtbacktest.LoadPanel(arch, start, end, cfg)
		must(err)
		fmt.Fprintf(os.Stderr, "panel: %d rows, %d days (%.1fs)\n", len(panel.Rows), len(panel.Days), time.Since(t0).Seconds())
		must(writePanelCSV(panel, filepath.Join(outDir, "panel.csv")))
		must(extractFeatures(start, end))
		return
	}
	if *analyze {
		must(runAnalysis(cfg, *only))
	}
}

func writePanelCSV(panel *dtbacktest.Panel, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"d", "code", "o", "c", "prev_close", "next_open", "vol20", "gap", "limit_low", "limit_high", "eligible", "short_eligible"})
	fs := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	fp := func(v *float64) string {
		if v == nil {
			return ""
		}
		return fs(*v)
	}
	for _, r := range panel.Rows {
		_ = w.Write([]string{r.Date.Format(dayLayout), r.Code, fs(r.Open), fs(r.Close), fs(r.PrevClose), fp(r.NextOpen), fp(r.Vol20),
			fs(r.Gap), fs(r.LimitLow), fs(r.LimitHigh), strconv.FormatBool(r.Eligible), strconv.FormatBool(r.ShortEligible)})
	}
	w.Flush()
	return w.Error()
}

// extractFeatures はティックから (日, 銘柄) ごとの特徴量を作る。母集団の行だけに絞り、
// 時間帯は前場全体（9:00〜11:30）と 15:00〜 を読む。
//
// 最初は 9:00〜9:11 に絞っていたが、それより後に寄る銘柄（特別気配が長い＝ギャップが大きい）が
// 特徴量から落ち、順位の計算からも外れていた（ロング上位 3 の 10%、ショート上位 3 の 36%）。
func extractFeatures(start, end time.Time) error {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var files []string
	all, _ := filepath.Glob(filepath.Join(jqDir, "equities_trades", "*.parquet"))
	for _, p := range all {
		day := strings.TrimSuffix(filepath.Base(p), ".parquet")
		if day >= start.Format(dayLayout) && day <= end.Format(dayLayout) {
			files = append(files, "'"+p+"'")
		}
	}
	fmt.Fprintf(os.Stderr, "tick files: %d\n", len(files))

	var dailyFiles []string
	allDaily, _ := filepath.Glob(filepath.Join(jqDir, "equities_bars_daily", "*.parquet"))
	for _, p := range allDaily {
		m := strings.TrimSuffix(filepath.Base(p), ".parquet")
		if m >= start.AddDate(0, -2, 0).Format("2006-01") && m <= end.Format("2006-01") {
			dailyFiles = append(dailyFiles, "'"+p+"'")
		}
	}

	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE panel AS SELECT * FROM read_csv('%s', header=true, types={'code': 'VARCHAR'})`, filepath.Join(outDir, "panel.csv"))); err != nil {
		return err
	}

	var cols []string
	for _, s := range fillSecs {
		cols = append(cols, fmt.Sprintf("arg_min(p, s) FILTER (WHERE ses = '01' AND s >= %d) AS f_%d", s, s))
	}
	for _, s := range markSecs {
		cols = append(cols, fmt.Sprintf("arg_max(p, s) FILTER (WHERE ses = '01' AND s <= %d) AS c_%d", s, s))
		cols = append(cols, fmt.Sprintf("sum(v) FILTER (WHERE ses = '01' AND s <= %d) AS vo_%d", s, s))
		cols = append(cols, fmt.Sprintf("count(*) FILTER (WHERE ses = '01' AND s <= %d) AS n_%d", s, s))
	}
	for _, t := range closeMarks {
		cols = append(cols, fmt.Sprintf("arg_max(p, tm) FILTER (WHERE tm < '%s') AS c_%s", t, tkey(t)))
	}
	for _, t := range closeFills {
		cols = append(cols, fmt.Sprintf("arg_min(p, tm) FILTER (WHERE tm >= '%s') AS f_%s", t, tkey(t)))
	}
	q := fmt.Sprintf(`
CREATE TABLE feats AS
WITH tk AS (
  SELECT t."Date" AS d, t."Code" AS code, t."Price" AS p, t."TradingVolume" AS v, t."Time" AS tm, t."SessionDistinction" AS ses,
         epoch(CAST('2000-01-01 ' || t."Time" AS TIMESTAMP)) - epoch(TIMESTAMP '2000-01-01 09:00:00') AS s
  FROM read_parquet([%s], union_by_name=true) t
  JOIN (SELECT DISTINCT d, code FROM panel) pn ON pn.d = t."Date" AND pn.code = t."Code"
  WHERE t."Time" < '11:30:01' OR t."Time" >= '15:00'
),
first AS (SELECT d, code, min(s) AS s0 FROM tk WHERE ses = '01' GROUP BY d, code),
agg AS (
  SELECT tk.d, tk.code,
    any_value(f.s0) AS s_first,
    arg_min(p, s) FILTER (WHERE ses = '01') AS p_first,
    sum(v) FILTER (WHERE ses = '01' AND s < f.s0 + 0.5) AS vo_auct,
    count(*) FILTER (WHERE ses = '01' AND s < f.s0 + 0.5) AS n_auct,
    sum(v) FILTER (WHERE ses = '01' AND s >= f.s0 AND s < f.s0 + 60) AS vo_a60,
    arg_max(p, s) FILTER (WHERE ses = '01' AND s < f.s0 + 30) AS c_a30,
    arg_max(p, s) FILTER (WHERE ses = '01' AND s < f.s0 + 60) AS c_a60,
    max(p) FILTER (WHERE ses = '01' AND s <= 60) AS h_60, min(p) FILTER (WHERE ses = '01' AND s <= 60) AS l_60,
    sum(p * v) FILTER (WHERE ses = '01' AND s <= 60) / nullif(sum(v) FILTER (WHERE ses = '01' AND s <= 60), 0) AS vwap_60,
    %s,
    arg_max(p, tm) FILTER (WHERE ses = '02') AS p_close,
    sum(v) FILTER (WHERE tm >= '15:30') AS vo_cauct,
    sum(v) FILTER (WHERE tm >= '15:10' AND tm < '15:25') AS vo_1510_25,
    count(*) FILTER (WHERE tm >= '15:10' AND tm < '15:25') AS n_1510_25,
    max(p) FILTER (WHERE tm >= '15:10' AND tm < '15:25') AS h_1510_25,
    min(p) FILTER (WHERE tm >= '15:10' AND tm < '15:25') AS l_1510_25,
    sum(p * v) FILTER (WHERE tm >= '15:10' AND tm < '15:25') / nullif(sum(v) FILTER (WHERE tm >= '15:10' AND tm < '15:25'), 0) AS vwap_1510_25
  FROM tk JOIN first f ON f.d = tk.d AND f.code = tk.code
  GROUP BY tk.d, tk.code
),
dbars AS (
  SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code, TRY_CAST("Vo" AS DOUBLE) AS vo
  FROM read_parquet([%s], union_by_name=true)
),
avgvo AS (
  SELECT d, code, avg(vo) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) AS avg_vo20
  FROM dbars
)
SELECT a.*, v.avg_vo20
FROM agg a LEFT JOIN avgvo v ON v.d = a.d AND v.code = a.code`,
		strings.Join(files, ","), strings.Join(cols, ",\n    "), strings.Join(dailyFiles, ","))
	t0 := time.Now()
	if _, err := db.Exec(q); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "features built (%.1fs)\n", time.Since(t0).Seconds())
	_, err = db.Exec(fmt.Sprintf(`COPY feats TO '%s' (FORMAT PARQUET)`, filepath.Join(outDir, "features.parquet")))
	return err
}

// -- 分析 ---------------------------------------------------------------

type table struct {
	name, title, sql string
}

func runAnalysis(cfg dtconfig.Config, only string) error {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return err
	}
	defer db.Close()
	longMax, _ := cfg.Signal.MaxGap.Float64()
	shortMin, _ := cfg.Margin.MinGap.Float64()
	shortMax, _ := cfg.Margin.MaxGap.Float64()

	setup := []string{
		fmt.Sprintf(`CREATE TABLE panel AS SELECT * FROM read_csv('%s', header=true, types={'code': 'VARCHAR'})`, filepath.Join(outDir, "panel.csv")),
		fmt.Sprintf(`CREATE TABLE feats AS SELECT * FROM read_parquet('%s')`, filepath.Join(outDir, "features.parquet")),
		// base: 母集団 × 特徴量。leg は L（ロング候補）/ S（ショート候補）。rk は日内の順位（ギャップ順）
		fmt.Sprintf(`CREATE TABLE base AS
WITH j AS (
  SELECT p.*, f.* EXCLUDE (d, code), year(p.d) AS yr
  FROM panel p JOIN feats f ON f.d = p.d AND f.code = p.code
  WHERE f.p_first IS NOT NULL
),
l AS (
  SELECT *, 'L' AS leg, row_number() OVER (PARTITION BY d ORDER BY gap) AS rk
  FROM j WHERE eligible AND gap < %g AND o > limit_low
),
s AS (
  SELECT *, 'S' AS leg, row_number() OVER (PARTITION BY d ORDER BY gap DESC) AS rk
  FROM j WHERE short_eligible AND gap >= %g AND gap < %g AND o < limit_high
)
SELECT *,
  CASE WHEN s_first < 1 THEN 'a <1s' WHEN s_first < 10 THEN 'b 1-10s' WHEN s_first < 60 THEN 'c 10-60s'
       WHEN s_first < 300 THEN 'd 1-5m' WHEN s_first < 600 THEN 'e 5-10m' WHEN s_first < 900 THEN 'f 10-15m' WHEN s_first < 1200 THEN 'g 15-20m' WHEN s_first < 1800 THEN 'h 20-30m' ELSE 'i >30m' END AS delay,
  -- ret: 建値 → 15:20 成行（bp）。ロングは買い、ショートは売り
  CASE WHEN leg = 'L' THEN (f_1520 / p_first - 1) ELSE (p_first / f_1520 - 1) END * 1e4 AS ret,
  CASE WHEN leg = 'L' THEN 1 ELSE -1 END AS sgn,
  vo_auct / nullif(avg_vo20, 0) AS auct_ratio,
  vo_60 / nullif(avg_vo20, 0) AS vo60_ratio,
  vo_cauct / nullif(avg_vo20, 0) AS cauct_ratio
FROM (SELECT * FROM l UNION ALL SELECT * FROM s)`, longMax, shortMin, shortMax),
	}
	for _, q := range setup {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("setup: %w\n%s", err, q)
		}
	}

	stat := func(expr string) string {
		return fmt.Sprintf("count(*) AS n, round(avg(%[1]s), 1) AS mean_bp, round(median(%[1]s), 1) AS med_bp, round(avg(%[1]s) / nullif(stddev(%[1]s), 0) * sqrt(count(*)), 2) AS t, round(avg(CASE WHEN %[1]s > 0 THEN 1 ELSE 0 END) * 100, 1) AS win_pct", expr)
	}
	tables := []table{
		{"sanity", "健全性: ティックの初値 = 日足の寄付か、母集団の大きさ", `
SELECT leg, count(*) AS n, count(DISTINCT d) AS days,
  round(avg(CASE WHEN abs(p_first - o) < 1e-6 THEN 1 ELSE 0 END) * 100, 2) AS first_eq_open_pct,
  round(avg(CASE WHEN f_1520 IS NULL THEN 1 ELSE 0 END) * 100, 2) AS no_1520_pct
FROM base GROUP BY leg ORDER BY leg`},
		{"delay", "発見 1: 寄付の遅れ（秒）× 建値→15:20 の bp（上位 10 / 上位 3）", `
SELECT leg, CASE WHEN rk <= 3 THEN 'top3' ELSE 'top10' END AS grp, delay, ` + stat("ret") + `
FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL GROUP BY 1, 2, 3 ORDER BY 1, 2 DESC, 3`},
		{"delay_year", "発見 1 の年別（上位 10）", `
SELECT leg, yr, delay, ` + stat("ret") + `
FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`},
		{"delay_fine", "発見 1 の細分: 1 秒未満の中身（ms）と 1〜60 秒", `
SELECT leg,
  CASE WHEN s_first < 0.1 THEN 'a <0.1s' WHEN s_first < 0.3 THEN 'b 0.1-0.3s' WHEN s_first < 1 THEN 'c 0.3-1s'
       WHEN s_first < 3 THEN 'd 1-3s' WHEN s_first < 10 THEN 'e 3-10s' WHEN s_first < 30 THEN 'f 10-30s' WHEN s_first < 60 THEN 'g 30-60s' ELSE 'h >=60s' END AS delay2,
  ` + stat("ret") + `
FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL GROUP BY 1, 2 ORDER BY 1, 2`},
		{"auct", "発見 2: 板寄せの出来高 ÷ 20 日平均出来高（5 分位）× bp。全体と、09:00 に寄った群の中で", `
WITH q AS (
  SELECT *, ntile(5) OVER (PARTITION BY leg ORDER BY auct_ratio) AS q5,
         ntile(5) OVER (PARTITION BY leg, s_first < 1 ORDER BY auct_ratio) AS q5_in
  FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL AND auct_ratio IS NOT NULL
)
SELECT leg, 'all' AS grp, q5 AS q, round(median(auct_ratio) * 100, 1) AS ratio_pct, ` + stat("ret") + ` FROM q GROUP BY 1, 2, 3
UNION ALL
SELECT leg, CASE WHEN s_first < 1 THEN 'opened' ELSE 'delayed' END, q5_in, round(median(auct_ratio) * 100, 1), ` + stat("ret") + ` FROM q GROUP BY 1, 2, 3
ORDER BY 1, 2, 3`},
		{"auct_year", "発見 2 の年別: 板寄せ出来高比（3 分位）× bp。ショート全体とロングの 09:00 に寄った群", `
WITH q AS (
  SELECT *, ntile(3) OVER (PARTITION BY leg, yr ORDER BY auct_ratio) AS t3
  FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL AND auct_ratio IS NOT NULL AND (leg = 'S' OR s_first < 1)
)
SELECT leg, yr, t3, round(median(auct_ratio) * 100, 1) AS ratio_pct, ` + stat("ret") + ` FROM q GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`},
		{"late_share", "候補のうち 09:10 より後に寄る銘柄の割合（残す時間帯の判断用）", `
SELECT leg, count(*) AS n,
  round(avg(CASE WHEN s_first >= 600 THEN 1 ELSE 0 END) * 100, 2) AS after_0910_pct,
  round(avg(CASE WHEN s_first >= 300 THEN 1 ELSE 0 END) * 100, 2) AS after_0905_pct,
  round(avg(CASE WHEN s_first >= 900 THEN 1 ELSE 0 END) * 100, 2) AS after_0915_pct,
  round(avg(CASE WHEN s_first >= 1800 THEN 1 ELSE 0 END) * 100, 2) AS after_0930_pct,
  round(quantile_cont(s_first, 0.99), 0) AS p99_sec, round(max(s_first), 0) AS max_sec
FROM base WHERE rk <= 10 GROUP BY 1
UNION ALL
SELECT leg || ' top3', count(*), round(avg(CASE WHEN s_first >= 600 THEN 1 ELSE 0 END) * 100, 2), round(avg(CASE WHEN s_first >= 300 THEN 1 ELSE 0 END) * 100, 2),
  round(avg(CASE WHEN s_first >= 900 THEN 1 ELSE 0 END) * 100, 2), round(avg(CASE WHEN s_first >= 1800 THEN 1 ELSE 0 END) * 100, 2),
  round(quantile_cont(s_first, 0.99), 0), round(max(s_first), 0)
FROM base WHERE rk <= 3 GROUP BY 1 ORDER BY 1`},
		{"entry_secs", "発見 3: 9:00 から X 秒後に成行を出したときの建値→15:20（上位 10、群別）。0 = 板寄せ", `
WITH u AS (
  SELECT leg, CASE WHEN s_first < 1 THEN 'opened' ELSE 'delayed' END AS grp, sgn, f_1520, p_first,
    f_5, f_10, f_20, f_30, f_45, f_60, f_90, f_120, f_180, f_300, f_600, f_900, f_1200, f_1800
  FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL
),
x AS (
  SELECT leg, grp, k, round(avg(sgn * (f_1520 / v - 1) * 1e4), 1) AS mean_bp, count(v) AS n
  FROM u UNPIVOT (v FOR k IN (p_first AS s0, f_5 AS s5, f_10 AS s10, f_20 AS s20, f_30 AS s30, f_45 AS s45, f_60 AS s60, f_90 AS s90, f_120 AS s120, f_180 AS s180, f_300 AS s300, f_600 AS s600, f_900 AS s900, f_1200 AS s1200, f_1800 AS s1800))
  GROUP BY 1, 2, 3
)
SELECT leg, grp, k, mean_bp, n FROM x ORDER BY leg, grp, CAST(substr(k, 2) AS INT)`},
		{"slip_secs", "発見 3 の裏: 09:00 に寄った銘柄を X 秒後に成行で建てると、板寄せ値より何 bp 不利か（上位 10）", `
WITH u AS (SELECT leg, sgn, p_first, f_5, f_10, f_20, f_30, f_45, f_60, f_90, f_120, f_180, f_300, f_600 FROM base WHERE rk <= 10 AND s_first < 1)
SELECT leg, k, round(avg(sgn * (v / p_first - 1) * 1e4), 1) AS worse_bp, round(median(sgn * (v / p_first - 1) * 1e4), 1) AS med_bp, count(v) AS n
FROM u UNPIVOT (v FOR k IN (f_5 AS s5, f_10 AS s10, f_20 AS s20, f_30 AS s30, f_45 AS s45, f_60 AS s60, f_90 AS s90, f_120 AS s120, f_180 AS s180, f_300 AS s300, f_600 AS s600))
GROUP BY 1, 2 ORDER BY leg, CAST(substr(k, 2) AS INT)`},
		{"path", "発見 4: 寄付後の初動（寄付→+30 秒 / +60 秒の向き、3 分位）で、その後（+60 秒→15:20）が変わるか", `
WITH u AS (
  SELECT leg, CASE WHEN s_first < 1 THEN 'opened' ELSE 'delayed' END AS grp,
    sgn * (c_a30 / p_first - 1) * 1e4 AS m30, sgn * (c_a60 / p_first - 1) * 1e4 AS m60,
    sgn * (f_1520 / c_a60 - 1) * 1e4 AS after60
  FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL AND c_a60 IS NOT NULL
),
q AS (SELECT *, ntile(3) OVER (PARTITION BY leg, grp ORDER BY m30) AS t30, ntile(3) OVER (PARTITION BY leg, grp ORDER BY m60) AS t60 FROM u)
SELECT leg, grp, 'm30' AS by, t30 AS tercile, round(median(m30), 1) AS move_med_bp, ` + stat("after60") + ` FROM q GROUP BY 1, 2, 3, 4
UNION ALL
SELECT leg, grp, 'm60', t60, round(median(m60), 1), ` + stat("after60") + ` FROM q GROUP BY 1, 2, 3, 4
ORDER BY 1, 2, 3, 4`},
		{"vo60", "発見 4b: 最初の 60 秒の出来高 ÷ 20 日平均（5 分位）× bp（上位 10、群別）", `
WITH q AS (SELECT *, ntile(5) OVER (PARTITION BY leg, s_first < 1 ORDER BY vo60_ratio) AS q5 FROM base WHERE rk <= 10 AND f_1520 IS NOT NULL AND vo60_ratio IS NOT NULL)
SELECT leg, CASE WHEN s_first < 1 THEN 'opened' ELSE 'delayed' END AS grp, q5, round(median(vo60_ratio) * 100, 1) AS ratio_pct, ` + stat("ret") + `
FROM q GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`},
		{"exit", "発見 5: 出口の区間ごとの bp（建玉の向きで符号を揃える。上位 3）", `
WITH u AS (
  SELECT leg, yr,
    sgn * (f_1520 / c_1510 - 1) * 1e4 AS r_1510_1520,
    sgn * (f_1524 / f_1520 - 1) * 1e4 AS r_1520_1524,
    sgn * (p_close / f_1524 - 1) * 1e4 AS r_1524_close,
    sgn * (p_close / f_1520 - 1) * 1e4 AS r_1520_close,
    sgn * (f_1520 / f_1500 - 1) * 1e4 AS r_1500_1520
  FROM base WHERE rk <= 3 AND f_1520 IS NOT NULL AND p_close IS NOT NULL AND f_1524 IS NOT NULL AND c_1510 IS NOT NULL
)
SELECT leg, 'all' AS yr, count(*) AS n,
  round(avg(r_1500_1520), 1) AS r1500_1520, round(avg(r_1510_1520), 1) AS r1510_1520, round(avg(r_1520_1524), 1) AS r1520_1524,
  round(avg(r_1524_close), 1) AS r1524_close, round(avg(r_1520_close), 1) AS r1520_close,
  round(avg(r_1520_close) / nullif(stddev(r_1520_close), 0) * sqrt(count(*)), 2) AS t_1520_close
FROM u GROUP BY 1
UNION ALL
SELECT leg, CAST(yr AS VARCHAR), count(*), round(avg(r_1500_1520), 1), round(avg(r_1510_1520), 1), round(avg(r_1520_1524), 1), round(avg(r_1524_close), 1), round(avg(r_1520_close), 1),
  round(avg(r_1520_close) / nullif(stddev(r_1520_close), 0) * sqrt(count(*)), 2)
FROM u GROUP BY 1, yr ORDER BY 1, 2`},
		{"exit_cond", "発見 5b: 15:10→15:20 の動き（3 分位）で、15:20→引けの動きが変わるか（上位 3）", `
WITH u AS (
  SELECT leg, sgn * (c_1520 / c_1510 - 1) * 1e4 AS pre, sgn * (p_close / f_1520 - 1) * 1e4 AS post,
    sgn * (f_1524 / f_1520 - 1) * 1e4 AS post24
  FROM base WHERE rk <= 3 AND f_1520 IS NOT NULL AND p_close IS NOT NULL AND c_1510 IS NOT NULL AND c_1520 IS NOT NULL
),
q AS (SELECT *, ntile(3) OVER (PARTITION BY leg ORDER BY pre) AS t3 FROM u)
SELECT leg, t3, round(median(pre), 1) AS pre_med_bp, count(*) AS n, round(avg(post), 1) AS post_close_bp, round(avg(post24), 1) AS post_1524_bp,
  round(avg(post) / nullif(stddev(post), 0) * sqrt(count(*)), 2) AS t_close
FROM q GROUP BY 1, 2 ORDER BY 1, 2`},
		{"exit_vol", "発見 5c: 引けの板寄せの出来高 ÷ 20 日平均（5 分位）× 15:20→引け（上位 3）", `
WITH q AS (SELECT *, ntile(5) OVER (PARTITION BY leg ORDER BY cauct_ratio) AS q5 FROM base WHERE rk <= 3 AND f_1520 IS NOT NULL AND p_close IS NOT NULL AND cauct_ratio IS NOT NULL)
SELECT leg, q5, round(median(cauct_ratio) * 100, 1) AS ratio_pct, count(*) AS n, round(avg(sgn * (p_close / f_1520 - 1) * 1e4), 1) AS r1520_close_bp
FROM q GROUP BY 1, 2 ORDER BY 1, 2`},
		{"exit_day", "発見 5d: 当日の含み（寄付→15:10）3 分位で、15:20→引けが変わるか（上位 3）", `
WITH u AS (SELECT leg, sgn * (c_1510 / p_first - 1) * 1e4 AS day, sgn * (p_close / f_1520 - 1) * 1e4 AS post FROM base WHERE rk <= 3 AND f_1520 IS NOT NULL AND p_close IS NOT NULL AND c_1510 IS NOT NULL),
q AS (SELECT *, ntile(3) OVER (PARTITION BY leg ORDER BY day) AS t3 FROM u)
SELECT leg, t3, round(median(day), 1) AS day_med_bp, count(*) AS n, round(avg(post), 1) AS post_close_bp, round(avg(post) / nullif(stddev(post), 0) * sqrt(count(*)), 2) AS t
FROM q GROUP BY 1, 2 ORDER BY 1, 2`},
	}
	wanted := map[string]bool{}
	for _, n := range strings.Split(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			wanted[n] = true
		}
	}
	for _, t := range tables {
		if len(wanted) > 0 && !wanted[t.name] {
			continue
		}
		fmt.Printf("\n## %s — %s\n\n", t.name, t.title)
		if err := printQuery(db, t.sql); err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
	}
	return nil
}

func printQuery(db *sql.DB, q string) error {
	rows, err := db.Query(q)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]string
	out = append(out, cols)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		line := make([]string, len(cols))
		for i, v := range vals {
			line[i] = cellOf(v)
		}
		out = append(out, line)
	}
	widths := make([]int, len(cols))
	for _, line := range out {
		for i, c := range line {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for r, line := range out {
		parts := make([]string, len(cols))
		for i, c := range line {
			parts[i] = fmt.Sprintf("%-*s", widths[i], c)
		}
		fmt.Println("| " + strings.Join(parts, " | ") + " |")
		if r == 0 {
			seps := make([]string, len(cols))
			for i := range cols {
				seps[i] = strings.Repeat("-", widths[i])
			}
			fmt.Println("| " + strings.Join(seps, " | ") + " |")
		}
	}
	return rows.Err()
}

func cellOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case time.Time:
		return x.Format(dayLayout)
	default:
		return fmt.Sprint(x)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
