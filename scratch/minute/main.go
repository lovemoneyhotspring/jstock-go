// 使い捨ての検証プログラム（分足で gap 戦略の入口・出口・フィルタを検証する）。
//
//	go run ./scratch/minute -extract            パネル + 分足の特徴量を /tmp/minute に書く
//	go run ./scratch/minute -sim                特徴量を読み、入口 × 出口の格子で検証する
//	go run ./scratch/minute -sim -filter long_bounce ...
package main

import (
	"database/sql"
	"encoding/csv"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	dtbacktest "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/backtest"
	dtconfig "github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/usmarket"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/storage"
)

const outDir = "/tmp/minute"
const jqDir = "data/jquants"
const dayLayout = "2006-01-02"

// 約定（その時刻以降の最初の約定値）を取る時刻と、値洗い（その時刻以前の最後の約定値）を取る時刻
var fillTimes = []string{"09:01", "09:02", "09:03", "09:05", "09:10", "09:15", "09:20", "09:30", "09:45", "10:00", "10:30", "11:00", "11:25", "12:30", "13:00", "13:30", "14:00", "14:30", "14:45", "15:00", "15:10", "15:15", "15:20", "15:24"}
var markTimes = []string{"09:00", "09:01", "09:02", "09:03", "09:05", "09:10", "09:15", "09:20", "09:30", "09:45", "10:00", "10:30", "11:00", "11:30", "12:30", "13:00", "13:30", "14:00", "14:30", "14:45", "15:00", "15:10", "15:15", "15:20", "15:24", "15:30"}

func tkey(t string) string { return strings.ReplaceAll(t, ":", "") }

type feat struct {
	fill   map[string]float64 // f_HHMM: その時刻以降の最初の約定値（成行の建値）
	mark   map[string]float64 // c_HHMM: その時刻以前の最後の約定値
	vo0900 float64
	vo0905 float64
	voDay  float64
	avgVo  float64 // 前 20 日の平均出来高
	h0905  float64
	l0905  float64
	vwap05 float64
	tLow   string
	tHigh  string
	tFirst string
	voFirst float64
}

func main() {
	var (
		configDir = flag.String("config-dir", "config/daytrade_margin", "")
		since     = flag.String("since", "2024-11-05", "")
		until     = flag.String("until", "2026-09-04", "")
		extract   = flag.Bool("extract", false, "")
		panelOnly = flag.Bool("panel-only", false, "パネルの CSV だけ書く（分足の特徴量を作らない）")
		sim       = flag.Bool("sim", false, "")
		filters   = flag.String("filter", "", "カンマ区切りのフィルタ名")
		entries   = flag.String("entries", "open,09:01,09:02,09:03,09:05,09:10,09:15,09:30,10:00", "")
		exits     = flag.String("exits", "close,15:24,15:20,15:10,15:00,14:30,14:00,13:00,11:25", "")
		dump      = flag.String("dump", "", "取引を CSV に書く（entry|exit を指定。例 open|close）")
		tag       = flag.String("tag", "", "出力ファイルの接尾")
		dropLong  = flag.String("drop-long", "", "ロング母集団から落とす (d,code) の CSV")
		dropShort = flag.String("drop-short", "", "ショート母集団から落とす (d,code) の CSV")
		yearly    = flag.Bool("yearly", false, "年別も出す")
		daily     = flag.Bool("daily", false, "分足を使わず日足の寄付・引けで（10 年の検証用）")
		ratioDrop = flag.Float64("ratio-drop", 0, "信用倍率（買残/売残）がこれ以上のロング候補を落とす（週次 margin-interest、判定日の 5 日以上前の最新）")
	)
	flag.Parse()
	start, _ := time.Parse(dayLayout, *since)
	end, _ := time.Parse(dayLayout, *until)
	cfg, err := dtconfig.Load(*configDir)
	must(err)
	arch := archive.NewArchive(jqDir)

	t0 := time.Now()
	panel, err := dtbacktest.LoadPanel(arch, start, end, cfg)
	must(err)
	fmt.Fprintf(os.Stderr, "panel: %d rows, %d days (%.1fs)\n", len(panel.Rows), len(panel.Days), time.Since(t0).Seconds())

	if *extract || *panelOnly {
		name := "panel.csv"
		if *panelOnly {
			name = "panel10.csv"
		}
		must(writePanelCSV(panel, filepath.Join(outDir, name)))
		if !*panelOnly {
			must(extractFeatures(start, end))
		}
		return
	}
	if !*sim {
		return
	}
	var feats map[string]*feat
	if !*daily {
		feats, err = readFeatures(filepath.Join(outDir, "features.parquet"))
		must(err)
		fmt.Fprintf(os.Stderr, "features: %d\n", len(feats))
	}
	if *ratioDrop > 0 {
		applyRatioDrop(panel, *ratioDrop)
	}

	signals, err := dtbacktest.SignalsFor(arch, cfg, panel, usmarket.NewFredFetcher(), "data/daytrade/us.json")
	must(err)

	if *dropLong != "" {
		applyDropList(panel, *dropLong, true)
	}
	if *dropShort != "" {
		applyDropList(panel, *dropShort, false)
	}
	if *filters != "" {
		for _, name := range strings.Split(*filters, ",") {
			applyFilter(panel, feats, strings.TrimSpace(name))
		}
	}

	mkFill := func(e, x string) dtbacktest.FillModel {
		if *daily {
			return dtbacktest.OpenCloseFill{}
		}
		return timeFill{feats, e, x}
	}
	if *dump != "" {
		parts := strings.Split(*dump, "|")
		res, err := dtbacktest.SimulateMarginWith(panel, cfg, signals, dtbacktest.Options{Fill: mkFill(parts[0], parts[1])})
		must(err)
		must(writeTrades(res, filepath.Join(outDir, "trades"+*tag+".csv")))
		printRes(parts[0], parts[1], res)
		return
	}

	fmt.Printf("%-6s %-6s | %10s %6s %9s | %10s %6s %9s %5s | %10s %6s %9s\n",
		"entry", "exit", "total", "shrp", "mdd", "long", "shrp", "mdd", "cnt", "short", "shrp", "mdd")
	for _, e := range strings.Split(*entries, ",") {
		for _, x := range strings.Split(*exits, ",") {
			res, err := dtbacktest.SimulateMarginWith(panel, cfg, signals, dtbacktest.Options{Fill: mkFill(e, x)})
			must(err)
			printRes(e, x, res)
			if *yearly {
				for _, y := range dtbacktest.YearlyOf(res.Daily) {
					fmt.Printf("    %d: 合算 %10.0f ロング %10.0f ショート %10.0f 取引日 %d\n", y.Year, y.PnL, y.LongPnL, y.ShortPnL, y.Traded)
				}
			}
		}
	}
}

// applyRatioDrop は信用倍率が th 以上のロング候補を落とす（DuckDB の ASOF で週次残高を引く）。
func applyRatioDrop(panel *dtbacktest.Panel, th float64) {
	tmp := filepath.Join(outDir, "panel_ratio.csv")
	must(writePanelCSV(panel, tmp))
	db, err := storage.OpenDuckDB()
	must(err)
	defer db.Close()
	rows, err := db.Query(fmt.Sprintf(`
WITH mi AS (
  SELECT "Date" AS md, CAST("Code" AS VARCHAR) AS code,
         TRY_CAST("LongVol" AS DOUBLE) / nullif(TRY_CAST("ShrtVol" AS DOUBLE), 0) AS ratio
  FROM read_parquet('%s/markets_margin_interest/*.parquet', union_by_name=true)
),
p AS (SELECT d, CAST(code AS VARCHAR) AS code FROM read_csv('%s', header=true, types={'code': 'VARCHAR'}) WHERE eligible)
SELECT p.d, p.code FROM p ASOF JOIN mi ON mi.code = p.code AND mi.md <= p.d - INTERVAL 5 DAY
WHERE mi.ratio >= %f`, jqDir, tmp, th))
	must(err)
	defer rows.Close()
	set := map[string]bool{}
	for rows.Next() {
		var d time.Time
		var code string
		must(rows.Scan(&d, &code))
		set[d.UTC().Format(dayLayout)+"|"+code] = true
	}
	dropped := 0
	for i := range panel.Rows {
		r := &panel.Rows[i]
		if r.Eligible && set[r.Date.Format(dayLayout)+"|"+r.Code] {
			r.Eligible = false
			dropped++
		}
	}
	fmt.Fprintf(os.Stderr, "ratio-drop >= %.0f: dropped %d\n", th, dropped)
}

// applyDropList は CSV（d,code）にある行を母集団から落とす。
func applyDropList(panel *dtbacktest.Panel, path string, long bool) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	must(err)
	set := map[string]bool{}
	for _, rec := range recs[1:] {
		set[rec[0]+"|"+rec[1]] = true
	}
	dropped := 0
	for i := range panel.Rows {
		r := &panel.Rows[i]
		if set[r.Date.Format(dayLayout)+"|"+r.Code] {
			if long {
				r.Eligible = false
			} else {
				r.ShortEligible = false
			}
			dropped++
		}
	}
	fmt.Fprintf(os.Stderr, "droplist %s: %d in list, dropped %d\n", path, len(set), dropped)
}

func printRes(e, x string, res *dtbacktest.MarginResult) {
	s, l, sh := res.Summary, res.LongSummary, res.ShortSummary
	fmt.Printf("%-6s %-6s | %10.0f %6.2f %9.0f | %10.0f %6.2f %9.0f %5d | %10.0f %6.2f %9.0f %5d\n",
		e, x, s.TotalPnL, s.Sharpe, s.MaxDrawdown, l.TotalPnL, l.Sharpe, l.MaxDrawdown, len(res.LongTrades),
		sh.TotalPnL, sh.Sharpe, sh.MaxDrawdown, len(res.ShortTrades))
}

// timeFill は分足で「T 以降の最初の約定値」で建て、「U 以降の最初の約定値」で手仕舞う。
type timeFill struct {
	feats       map[string]*feat
	entry, exit string
}

func (f timeFill) Fill(r dtbacktest.Row) (float64, float64, bool) {
	ft := f.feats[r.Date.Format(dayLayout)+"|"+r.Code]
	if ft == nil {
		return 0, 0, false
	}
	entry := r.Open
	if f.entry != "open" {
		v, ok := ft.fill[tkey(f.entry)]
		if !ok || v <= 0 {
			return 0, 0, false
		}
		entry = v
	}
	exit := r.Close
	// "10:30/15:20" のように書けば ロング/ショート で出口を分ける（脚はギャップの符号で見分ける）
	exitAt := f.exit
	if parts := strings.Split(exitAt, "/"); len(parts) == 2 {
		if r.Gap < 0 {
			exitAt = parts[0]
		} else {
			exitAt = parts[1]
		}
	}
	if exitAt != "close" {
		v, ok := ft.fill[tkey(exitAt)]
		if !ok || v <= 0 {
			// その時刻以降に約定が無ければ引け
			v = r.Close
		}
		exit = v
	}
	return entry, exit, entry > 0 && exit > 0
}

// applyFilter は母集団の行を落とす（Eligible / ShortEligible を偽にする）。
func applyFilter(panel *dtbacktest.Panel, feats map[string]*feat, name string) {
	dropped := 0
	for i := range panel.Rows {
		r := &panel.Rows[i]
		ft := feats[r.Date.Format(dayLayout)+"|"+r.Code]
		if ft == nil {
			continue
		}
		c05 := ft.mark["0905"]
		c01 := ft.mark["0901"]
		c03 := ft.mark["0903"]
		voRatio := 0.0
		if ft.avgVo > 0 {
			voRatio = ft.voFirst / ft.avgVo
		}
		opened01 := ft.tFirst != "" && ft.tFirst <= "09:01"
		opened03 := ft.tFirst != "" && ft.tFirst <= "09:03"
		opened05 := ft.tFirst != "" && ft.tFirst <= "09:05"
		late := ft.tFirst > "09:00"
		drop := false
		switch name {
		// ロング: 09:05 に寄付より上（反発している）ものだけ
		case "long_bounce":
			drop = !(c05 > 0 && c05 > r.Open)
		// ロング: 09:05 に寄付以下（下げ続け）ものだけ
		case "long_fall":
			drop = !(c05 > 0 && c05 <= r.Open)
		case "long_bounce03":
			drop = !(c03 > 0 && c03 > r.Open)
		case "long_fall03":
			drop = !(c03 > 0 && c03 <= r.Open)
		case "long_fall01":
			drop = !(c01 > 0 && c01 <= r.Open)
		// 寄っている銘柄のうち、反発している / 下げ続けているものを落とす（未約定は残す）
		case "long_drop_bounce01":
			drop = opened01 && c01 > r.Open
		case "long_drop_fall01":
			drop = opened01 && c01 <= r.Open
		case "long_drop_bounce03":
			drop = opened03 && c03 > r.Open
		case "long_drop_fall03":
			drop = opened03 && c03 <= r.Open
		case "long_drop_bounce05":
			drop = opened05 && c05 > r.Open
		case "long_drop_fall05":
			drop = opened05 && c05 <= r.Open
		case "short_drop_run05":
			drop = opened05 && c05 >= r.Open
		case "short_drop_fade05":
			drop = opened05 && c05 < r.Open
		// 09:00 に寄った（特別気配で遅れなかった）銘柄を落とす / 遅れた銘柄を落とす
		case "drop_on_time":
			drop = !late
		case "drop_late":
			drop = late
		case "long_bounce01":
			drop = !(c01 > 0 && c01 > r.Open)
		// 寄り付きの出来高が平時の 5% 以上 / 未満
		case "vo_hi":
			drop = !(voRatio >= 0.05)
		case "vo_lo":
			drop = !(voRatio < 0.05)
		case "vo_hi10":
			drop = !(voRatio >= 0.10)
		case "vo_lo10":
			drop = !(voRatio < 0.10)
		// 09:05 までに寄付より 1% 以上下（深い下げ）
		case "long_deep05":
			drop = !(ft.l0905 > 0 && ft.l0905/r.Open-1 <= -0.01)
		// ショート: 09:05 に寄付より下（失速）だけ
		case "short_fade":
			drop = !(c05 > 0 && c05 < r.Open)
		case "short_run":
			drop = !(c05 > 0 && c05 >= r.Open)
		default:
			must(fmt.Errorf("unknown filter %q", name))
		}
		if drop {
			// long_* はロング側だけ、short_* はショート側だけ、vo_* は両方
			if strings.HasPrefix(name, "long_") {
				r.Eligible = false
			} else if strings.HasPrefix(name, "short_") {
				r.ShortEligible = false
			} else {
				r.Eligible = false
				r.ShortEligible = false
			}
			dropped++
		}
	}
	fmt.Fprintf(os.Stderr, "filter %s: dropped %d\n", name, dropped)
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

func extractFeatures(start, end time.Time) error {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return err
	}
	defer db.Close()

	var files []string
	all, _ := filepath.Glob(filepath.Join(jqDir, "equities_bars_minute", "*.parquet"))
	for _, p := range all {
		day := strings.TrimSuffix(filepath.Base(p), ".parquet")
		if day >= start.Format(dayLayout) && day <= end.Format(dayLayout) {
			files = append(files, "'"+p+"'")
		}
	}
	fmt.Fprintf(os.Stderr, "minute files: %d\n", len(files))

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
	for _, t := range fillTimes {
		cols = append(cols, fmt.Sprintf("arg_min(O, t) FILTER (WHERE t >= '%s') AS f_%s", t, tkey(t)))
	}
	for _, t := range markTimes {
		cols = append(cols, fmt.Sprintf("arg_max(C, t) FILTER (WHERE t <= '%s') AS c_%s", t, tkey(t)))
	}
	q := fmt.Sprintf(`
CREATE TABLE feats AS
WITH m AS (
  SELECT m."Date" AS d, m."Code" AS code, m."Time" AS t, m."O" AS O, m."H" AS H, m."L" AS L, m."C" AS C, m."Vo" AS Vo, m."Va" AS Va
  FROM read_parquet([%s], union_by_name=true) m
  JOIN (SELECT DISTINCT d, code FROM panel) p ON p.d = m."Date" AND p.code = m."Code"
),
agg AS (
  SELECT d, code,
    %s,
    sum(Vo) FILTER (WHERE t = '09:00') AS vo_0900,
    sum(Vo) FILTER (WHERE t <= '09:05') AS vo_0905,
    sum(Vo) AS vo_day,
    max(H) FILTER (WHERE t <= '09:05') AS h_0905,
    min(L) FILTER (WHERE t <= '09:05') AS l_0905,
    sum(Va) FILTER (WHERE t <= '09:05') / nullif(sum(Vo) FILTER (WHERE t <= '09:05'), 0) AS vwap_0905,
    sum(Va) / nullif(sum(Vo), 0) AS vwap_day,
    min(t) AS t_first,
    arg_min(Vo, t) AS vo_first,
    arg_min(t, L) AS t_low,
    arg_max(t, H) AS t_high,
    max(H) AS hi, min(L) AS lo
  FROM m GROUP BY d, code
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
	if _, err := db.Exec(fmt.Sprintf(`COPY feats TO '%s' (FORMAT PARQUET)`, filepath.Join(outDir, "features.parquet"))); err != nil {
		return err
	}
	return nil
}

func readFeatures(path string) (map[string]*feat, error) {
	db, err := storage.OpenDuckDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(fmt.Sprintf(`SELECT * FROM read_parquet('%s')`, path))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	out := map[string]*feat{}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		ft := &feat{fill: map[string]float64{}, mark: map[string]float64{}}
		var d time.Time
		var code string
		for i, c := range cols {
			v := vals[i]
			switch {
			case c == "d":
				d = v.(time.Time)
			case c == "code":
				code = v.(string)
			case strings.HasPrefix(c, "f_"):
				ft.fill[c[2:]] = num(v)
			case strings.HasPrefix(c, "c_"):
				ft.mark[c[2:]] = num(v)
			case c == "vo_0900":
				ft.vo0900 = num(v)
			case c == "vo_0905":
				ft.vo0905 = num(v)
			case c == "vo_day":
				ft.voDay = num(v)
			case c == "avg_vo20":
				ft.avgVo = num(v)
			case c == "h_0905":
				ft.h0905 = num(v)
			case c == "l_0905":
				ft.l0905 = num(v)
			case c == "vwap_0905":
				ft.vwap05 = num(v)
			case c == "t_first":
				ft.tFirst, _ = v.(string)
			case c == "vo_first":
				ft.voFirst = num(v)
			case c == "t_low":
				ft.tLow, _ = v.(string)
			case c == "t_high":
				ft.tHigh, _ = v.(string)
			}
		}
		out[d.UTC().Format(dayLayout)+"|"+code] = ft
	}
	return out, rows.Err()
}

func num(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case float64:
		return x
	case float32:
		return float64(x)
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	case sql.NullFloat64:
		return x.Float64
	default:
		return math.NaN()
	}
}

func writeTrades(res *dtbacktest.MarginResult, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"side", "d", "code", "rank", "gap", "shares", "entry", "exit", "amount", "fees", "pnl", "scale", "carried"})
	fs := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	write := func(side string, trades []dtbacktest.Trade) {
		for _, t := range trades {
			_ = w.Write([]string{side, t.Date.Format(dayLayout), t.Code, strconv.Itoa(t.Rank), fs(t.Gap), fs(t.Shares), fs(t.Entry), fs(t.Exit),
				fs(t.Amount), fs(t.Fees), fs(t.PnL), fs(t.Scale), strconv.FormatBool(t.Carried)})
		}
	}
	write("BUY", res.LongTrades)
	write("SELL", res.ShortTrades)
	w.Flush()
	// 日次も
	g, err := os.Create(strings.TrimSuffix(path, ".csv") + "_daily.csv")
	if err != nil {
		return err
	}
	defer g.Close()
	dw := csv.NewWriter(g)
	_ = dw.Write([]string{"d", "pnl", "long_pnl", "short_pnl", "long_n", "short_n", "long_scale", "short_mul"})
	for _, d := range res.Daily {
		_ = dw.Write([]string{d.Date.Format(dayLayout), fs(d.PnL), fs(d.LongPnL), fs(d.ShortPnL), strconv.Itoa(d.LongN), strconv.Itoa(d.ShortN), fs(d.LongScale), fs(d.ShortMultiplier)})
	}
	dw.Flush()
	return w.Error()
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var _ = sort.Strings
