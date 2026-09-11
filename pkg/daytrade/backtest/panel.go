// Package backtest はアーカイブで同じ規則を検証する。
//
// 前夜の母集団の条件（universe）と 9:00 の順位付け（selection）を、そのまま 10 年ぶんの
// パネルに当てる。資金は固定（複利なし）、100 株単位、手数料は段階制（fees）。
// 研究ノートの表と同じ計算。
//
// パネルの組み立ては DuckDB の 1 本の SQL。Python 版は polars の窓関数
// （rolling_median / rolling_std / rank over Date）で書いていた部分で、Go に同等の
// 表計算が無い以上、窓関数を持つ SQL に落とすのが素直な移植になる。ここで作るのは
// 「前日までの情報だけで決まる特徴量」で、判断（順位・株数・ゲート）は Go 側で行う。
package backtest

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/universe"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
	"github.com/lovemoneyhotspring/jstock-go/pkg/wbcore/marketrules"
)

// TradingDays は 1 年の営業日数（年率換算）。
const TradingDays = 245

// PanelCacheEnabled は設定に依存しない部分の Parquet キャッシュを使うか
// （backtest の --no-cache で切る）。詳細は panelcache.go。
var PanelCacheEnabled = true

// Row はパネルの 1 行（ある日・ある銘柄）。
type Row struct {
	Date      time.Time
	Code      string
	Open      float64
	Close     float64
	PrevClose float64
	// NextOpen は翌営業日の寄付。ショートが引けストップ高で返済できなかったときの返済値。
	NextOpen *float64
	Vol20    *float64
	// EarnYield は益回り（直近の本決算の当期純利益 ÷ 前日の時価総額）。無ければ nil。
	EarnYield *float64
	// Sector は 33 業種コード（equities/master の S33）。同じ業種に建玉を偏らせない
	// 判定（signal.max_per_sector）に使う。取れなければ空。
	Sector string
	Gap    float64
	// LimitLow / LimitHigh は前日終値を基準値段とする制限値幅（ストップ安・高）。
	LimitLow      float64
	LimitHigh     float64
	Eligible      bool
	ShortEligible bool
}

// Panel は期間ぶんの行と、営業日の並び。
type Panel struct {
	Rows []Row
	// Days は期間の営業日（取引が無い日も日次の統計に並べるため）。
	Days []time.Time
}

// LoadPanel は (Date, Code) ごとの特徴量と当日の寄付・終値を作る。
// eligible / short_eligible のどちらかに入る行だけを返す（全銘柄 × 10 年を持つと
// メモリを食うだけで、判断に使わない）。
func LoadPanel(arch *archive.Archive, start, end time.Time, cfg config.Config) (*Panel, error) {
	lookback := start.AddDate(0, 0, -(cfg.Universe.TurnoverDays*2 + 10))
	barsSrc, ok := archsql.Source(arch, universe.EPBars, lookback, end)
	if !ok {
		return nil, fmt.Errorf("足がありません。jquants backfill / sync を先に")
	}
	masterSrc, ok := archsql.Source(arch, universe.EPMaster, lookback, end)
	if !ok {
		return nil, fmt.Errorf("銘柄一覧（equities/master）がありません。jquants backfill / sync を先に")
	}

	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// 本決算は年 1 回なので 420 日遡る（益回り・赤字の判定。前日引け後の決算フラグは
	// 直近 1 日しか見ないので、同じ読み込みで足りる）
	finsSrc, hasFins := archsql.Source(arch, universe.EPFins, start.AddDate(0, 0, -420), end)
	schedSrc, hasSched := archsql.Source(arch, universe.EPEarningsDate, start.AddDate(0, 0, -120), end)
	alertSrc, hasAlert := archsql.Source(arch, universe.EPMarginAlert, start.AddDate(0, 0, -7), end)

	query := buildPanelQuery(panelSources{
		bars: barsSrc, master: masterSrc,
		fins: finsSrc, hasFins: hasFins,
		sched: schedSrc, hasSched: hasSched,
		alert: alertSrc, hasAlert: hasAlert,
	}, start, end, cfg)

	// キャッシュがあれば（作れれば）そちらから読む。作れなくても検証は続ける
	// ——遅くなるだけで結果は同じなので、ここで止める理由が無い。
	if PanelCacheEnabled {
		if path, err := ensurePanelCache(db, arch, cfg, end); err != nil {
			fmt.Fprintf(os.Stderr, "パネルのキャッシュを使えません（そのまま実行します）: %v\n", err)
		} else {
			query = buildCachedPanelQuery(path, start, end, cfg)
		}
	}

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("パネルの組み立てに失敗しました: %w", err)
	}
	defer rows.Close()

	panel := &Panel{}
	for rows.Next() {
		var (
			r                 Row
			nextOpen, vol20   sql.NullFloat64
			earnYield         sql.NullFloat64
			sector            sql.NullString
			limitLow, limitHi sql.NullFloat64
		)
		if err := rows.Scan(&r.Date, &r.Code, &r.Open, &r.Close, &r.PrevClose,
			&nextOpen, &vol20, &earnYield, &sector, &r.Gap, &limitLow, &limitHi,
			&r.Eligible, &r.ShortEligible); err != nil {
			return nil, err
		}
		r.Date = r.Date.UTC()
		if nextOpen.Valid {
			v := nextOpen.Float64
			r.NextOpen = &v
		}
		if vol20.Valid {
			v := vol20.Float64
			r.Vol20 = &v
		}
		if earnYield.Valid {
			v := earnYield.Float64
			r.EarnYield = &v
		}
		r.Sector = sector.String
		r.LimitLow, r.LimitHigh = limitLow.Float64, limitHi.Float64
		panel.Rows = append(panel.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	days, err := tradingDays(db, barsSrc, start, end)
	if err != nil {
		return nil, err
	}
	panel.Days = days
	if len(panel.Days) == 0 {
		return nil, fmt.Errorf("対象日がありません（%s〜%s）",
			start.Format(archsql.DateLayout), end.Format(archsql.DateLayout))
	}
	return panel, nil
}

func tradingDays(db *sql.DB, barsSrc string, start, end time.Time) ([]time.Time, error) {
	query := fmt.Sprintf(`SELECT DISTINCT "Date" FROM %s WHERE "Date" >= %s AND "Date" <= %s ORDER BY 1`,
		barsSrc, archsql.Lit(start), archsql.Lit(end))
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d.UTC())
	}
	return out, rows.Err()
}

type panelSources struct {
	bars, master string
	fins         string
	hasFins      bool
	sched        string
	hasSched     bool
	alert        string
	hasAlert     bool
}

// buildPanelQuery はパネルの SQL を組み立てる。
//
// 決算・規制のフラグは「前営業日に起きたことが翌営業日に効く」ので、日付そのものでは
// なく営業日の連番（di）で 1 日ずらす。暦日で足すと連休明けに効かなくなる。
func buildPanelQuery(src panelSources, start, end time.Time, cfg config.Config) string {
	var b strings.Builder
	b.WriteString(panelCTEs(src, start, end, cfg))
	fmt.Fprintf(&b, `SELECT d, code, o, c, prev_close, next_open, vol20, earn_yield, sector,
       o / prev_close - 1 AS gap,
       prev_close - (%s) AS limit_low,
       prev_close + (%s) AS limit_high,
       (%s) AS eligible,
       (%s) AS short_eligible
FROM valued
WHERE prev_close IS NOT NULL AND prev_close > 0
  AND ((%s) OR (%s))
ORDER BY d, code`,
		limitWidthSQL("prev_close"), limitWidthSQL("prev_close"),
		eligibleSQL(cfg.Universe), shortEligibleSQL(cfg.Margin),
		eligibleSQL(cfg.Universe), shortEligibleSQL(cfg.Margin))
	return b.String()
}

// panelCTEs は WITH 句（bars … valued）。直接の経路とキャッシュ作成で共有する。
// start は joined の絞り込みにだけ効くので、キャッシュ作成では最古を渡す。
func panelCTEs(src panelSources, start, end time.Time, cfg config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
WITH bars AS (
  SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code,
         TRY_CAST("O" AS DOUBLE) AS o, TRY_CAST("C" AS DOUBLE) AS c,
         TRY_CAST("Va" AS DOUBLE) AS va,
         coalesce(nullif(TRY_CAST("AdjFactor" AS DOUBLE), 0), 1) AS af,
         TRY_CAST("MktCap" AS DOUBLE) AS cap
  FROM %s
  WHERE "Date" <= %s AND TRY_CAST("O" AS DOUBLE) > 0 AND TRY_CAST("C" AS DOUBLE) > 0
),
days AS (
  SELECT d, row_number() OVER (ORDER BY d) AS di FROM (SELECT DISTINCT d FROM bars)
),
-- 分割・併合の日（AdjFactor ≠ 1）は前日終値を調整前のまま使わない。前夜の plan は
-- その日の係数を知り得ないので、その日は建てない（prev_close を NULL にして外す）。
-- 日次リターン（ボラ）と翌寄り（張り付きの返済値）は係数で同じ水準に揃える
-- （J-Quants の AdjFactor は権利落ち日の行に入り、前日終値 × 係数 が比較できる値）。
lagged AS (
  SELECT b.*,
         CASE WHEN b.af = 1 THEN lag(b.c) OVER w END AS prev_close,
         lead(b.o) OVER w / lead(b.af) OVER w AS next_open,
         -- next_open_d は「次の足の日付」。キャッシュ経由で期間の終わりを再現するために要る
         -- （キャッシュは全期間で作るので、end 以降の足を見た next_open を無効にする）。
         lead(b.d) OVER w AS next_open_d,
         b.c / (lag(b.c) OVER w * b.af) - 1 AS ret,
         lag(b.cap) OVER w AS mkt_cap
  FROM bars b
  WINDOW w AS (PARTITION BY b.code ORDER BY b.d)
),
rolled AS (
  SELECT l.*,
         CASE WHEN count(l.va) OVER win = %d THEN median(l.va) OVER win END AS turnover_med,
         CASE WHEN count(l.ret) OVER vwin = %d THEN stddev_samp(l.ret) OVER vwin END AS vol20
  FROM lagged l
  WINDOW win AS (PARTITION BY l.code ORDER BY l.d ROWS BETWEEN %d PRECEDING AND 1 PRECEDING),
         vwin AS (PARTITION BY l.code ORDER BY l.d ROWS BETWEEN %d PRECEDING AND 1 PRECEDING)
),
master AS (
  SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code,
         %s AS segment,
         CAST("S33" AS VARCHAR) AS sector,
         CAST("ProdCat" AS VARCHAR) AS product,
         CAST("Mrgn" AS VARCHAR) = '2' AS shortable
  FROM %s
),
joined AS (
  SELECT r.*, m.segment, m.shortable, m.sector, dd.di
  FROM rolled r
  JOIN master m ON m.d = r.d AND m.code = r.code
  JOIN days dd ON dd.d = r.d
  WHERE r.d >= %s AND m.product = '%s'
),
`, src.bars, archsql.Lit(end),
		cfg.Universe.TurnoverDays, universe.VolDays,
		cfg.Universe.TurnoverDays, universe.VolDays,
		segmentSQL(`"MktNm"`), src.master,
		archsql.Lit(start), universe.StockProduct)

	// 決算（前日引け後）→ 翌営業日にフラグ
	if src.hasFins {
		fmt.Fprintf(&b, `earn AS (
  SELECT DISTINCT CAST(f."Code" AS VARCHAR) AS code, dd.di + 1 AS di
  FROM %s f JOIN days dd ON dd.d = f."DiscDate"
  WHERE f."DiscTime" IS NOT NULL AND %s
),
`, src.fins, postCloseSQL(`f."DiscDate"`, `f."DiscTime"`))
	} else {
		b.WriteString("earn AS (SELECT NULL::VARCHAR AS code, NULL::BIGINT AS di WHERE false),\n")
	}

	// 直近の本決算の当期純利益（益回り・赤字の判定）。開示日**より後**の日から効かせる
	// ——前夜の plan が使えるのは前日までに開示されたものだけ（universe.loadLatestNetProfit と同じ）。
	if src.hasFins {
		fmt.Fprintf(&b, `finsfy AS (
  SELECT CAST(f."Code" AS VARCHAR) AS code, f."DiscDate" AS fd,
         arg_max(TRY_CAST(f."NP" AS DOUBLE), CAST(f."DiscNo" AS VARCHAR)) AS np
  FROM %s f
  WHERE CAST(f."CurPerType" AS VARCHAR) = 'FY'
    AND CAST(f."DocType" AS VARCHAR) LIKE 'FYFinancialStatements%%'
    AND TRY_CAST(f."NP" AS DOUBLE) IS NOT NULL
  GROUP BY 1, 2
),
`, src.fins)
	} else {
		b.WriteString("finsfy AS (SELECT NULL::VARCHAR AS code, NULL::DATE AS fd, NULL::DOUBLE AS np WHERE false),\n")
	}

	// 当日開示の予定（SchDate）。予定が当日に出たものは前夜の判断材料にならない
	if src.hasSched {
		fmt.Fprintf(&b, `sched AS (
  SELECT DISTINCT CAST(s."Code" AS VARCHAR) AS code, s."SchDate" AS d
  FROM %s s WHERE s."PubDate" < s."SchDate"
),
`, src.sched)
	} else {
		b.WriteString("sched AS (SELECT NULL::VARCHAR AS code, NULL::DATE AS d WHERE false),\n")
	}

	// 信用規制の公表 → 翌営業日にフラグ（売り禁は別に持つ）
	if src.hasAlert {
		fmt.Fprintf(&b, `alerts AS (
  SELECT DISTINCT CAST(a."Code" AS VARCHAR) AS code, dd.di + 1 AS di,
         max(CASE WHEN a."PubReason" LIKE '%%"RestrictedByJSF": "1"%%' THEN 1 ELSE 0 END)
           OVER (PARTITION BY CAST(a."Code" AS VARCHAR), dd.di) = 1 AS jsf_stop
  FROM %s a JOIN days dd ON dd.d = a."PubDate"
),
`, src.alert)
	} else {
		b.WriteString("alerts AS (SELECT NULL::VARCHAR AS code, NULL::BIGINT AS di, false AS jsf_stop WHERE false),\n")
	}

	minTurnover, _ := cfg.Universe.MinTurnover.Float64()
	fmt.Fprintf(&b, `flagged AS (
  SELECT j.*,
         coalesce(e.code IS NOT NULL, false) AS earn_prev,
         coalesce(s.code IS NOT NULL, false) AS disc_today,
         coalesce(al.code IS NOT NULL, false) AS alert,
         coalesce(al.jsf_stop, false) AS jsf_stop
  FROM joined j
  LEFT JOIN earn e ON e.code = j.code AND e.di = j.di
  LEFT JOIN sched s ON s.code = j.code AND s.d = j.d
  LEFT JOIN alerts al ON al.code = j.code AND al.di = j.di
),
tercile AS (
  SELECT f.*,
         CASE WHEN f.turnover_med >= %f THEN
           least(3, greatest(1, CAST(ceil(
             row_number() OVER (PARTITION BY f.d
               ORDER BY CASE WHEN f.turnover_med >= %f THEN f.mkt_cap END NULLS LAST, f.code)
             * 3.0 / nullif(count(*) FILTER (WHERE f.turnover_med >= %f) OVER (PARTITION BY f.d), 0)
           ) AS INTEGER)))
         ELSE 0 END AS cap_tercile
  FROM flagged f
),
-- 本決算は「その日から %[4]d 日以内に開示されたもの」だけを見る。実運用の plan が
-- 判定日から遡って探すのと同じ窓にする（universe.FinsLookbackDays）。絶対の窓で切ると
-- --since によって同じ日の判定が変わり、キャッシュとも食い違う。
valued AS (
  SELECT t.*,
         np_fy / nullif(t.mkt_cap * 1e6, 0) AS earn_yield,
         coalesce(np_fy <= 0, false) AS is_loss
  FROM (
    SELECT t.*, CASE WHEN fy.fd >= t.d - INTERVAL %[4]d DAY THEN fy.np END AS np_fy
    FROM tercile t ASOF LEFT JOIN finsfy fy ON fy.code = t.code AND fy.fd < t.d
  ) t
)
`,
		minTurnover, minTurnover, minTurnover, universe.FinsLookbackDays)
	return b.String()
}

// panelCacheColumns はキャッシュに落とす列。設定に依存しないものだけを持ち、
// 設定に依存する判定（eligible / short_eligible・ギャップ・制限値幅）は読み出し側で当てる。
const panelCacheColumns = `SELECT d, code, o, c, prev_close, next_open, next_open_d, vol20, earn_yield,
       sector, segment, shortable, turnover_med, cap_tercile, earn_prev, disc_today, alert, jsf_stop, is_loss`

// buildCacheQuery はキャッシュに落とす行を作る SQL。期間は切らず（読み出し側で切る）、
// 流動性の下限だけで絞る——下限を満たさない行はどの設定でも母集団に入らないため。
// ロングとショートで下限が違う設定もありうるので、小さい方で残す。
func buildCacheQuery(src panelSources, end time.Time, cfg config.Config) string {
	var b strings.Builder
	b.WriteString(panelCTEs(src, time.Time{}, end, cfg))
	fmt.Fprintf(&b, `%s
FROM valued
WHERE prev_close IS NOT NULL AND prev_close > 0 AND turnover_med >= %f`,
		panelCacheColumns, cacheTurnoverFloor(cfg))
	return b.String()
}

// cacheTurnoverFloor はキャッシュに残す売買代金の下限（ロング・ショートの小さい方）。
func cacheTurnoverFloor(cfg config.Config) float64 {
	floor, _ := cfg.Universe.MinTurnover.Float64()
	if cfg.Margin.Enabled {
		if m, _ := cfg.Margin.MinTurnover.Float64(); m < floor {
			floor = m
		}
	}
	return floor
}

// buildCachedPanelQuery はキャッシュから読む SQL。直接の経路と同じ行・同じ値を返す。
//
// next_open は「次の足が end より後なら NULL」にする。キャッシュは全期間で作るので、
// そうしないと期間の終わりに未来の足が見えてしまう（上場廃止・売買停止で end より前に
// 足が途切れる銘柄でも同じ。営業日で切ると取りこぼす）。
func buildCachedPanelQuery(cachePath string, start, end time.Time, cfg config.Config) string {
	return fmt.Sprintf(`SELECT d, code, o, c, prev_close,
       CASE WHEN next_open_d > %[2]s THEN NULL ELSE next_open END AS next_open,
       vol20, earn_yield, sector,
       o / prev_close - 1 AS gap,
       prev_close - (%[3]s) AS limit_low,
       prev_close + (%[3]s) AS limit_high,
       (%[4]s) AS eligible,
       (%[5]s) AS short_eligible
FROM read_parquet(%[6]s)
WHERE d >= %[1]s AND d <= %[2]s
  AND ((%[4]s) OR (%[5]s))
ORDER BY d, code`,
		archsql.Lit(start), archsql.Lit(end), limitWidthSQL("prev_close"),
		eligibleSQL(cfg.Universe), shortEligibleSQL(cfg.Margin), archsql.LitString(cachePath))
}

// segmentSQL は市場区分名を prime / standard / growth / other に畳む
// （universe.SegmentOf の SQL 版。判定の順序も同じ）。
func segmentSQL(column string) string {
	return fmt.Sprintf(`CASE
  WHEN %[1]s LIKE '%%プライム%%' OR %[1]s LIKE '%%一部%%' THEN 'prime'
  WHEN %[1]s LIKE '%%グロース%%' OR %[1]s LIKE '%%マザーズ%%' THEN 'growth'
  WHEN %[1]s LIKE '%%スタンダード%%' OR %[1]s LIKE '%%二部%%' OR %[1]s LIKE '%%JASDAQ%%' THEN 'standard'
  ELSE 'other' END`, column)
}

// postCloseSQL は決算開示が引け後か（引け時刻は 2024-11-05 から 15:30）。
func postCloseSQL(dateColumn, timeColumn string) string {
	return fmt.Sprintf(`substr(CAST(%s AS VARCHAR), 1, 5) >= CASE WHEN %s < DATE '2024-11-05' THEN '15:00' ELSE '15:30' END`,
		timeColumn, dateColumn)
}

// limitWidthSQL は制限値幅（片側）。marketrules.PriceLimitTable の SQL 版。
func limitWidthSQL(column string) string {
	table := marketrules.PriceLimitTable()
	var b strings.Builder
	b.WriteString("CASE ")
	for i, entry := range table {
		bound, width := entry[0], entry[1]
		if i == len(table)-1 {
			fmt.Fprintf(&b, "ELSE %s END", width.String())
			break
		}
		fmt.Fprintf(&b, "WHEN %s < %s THEN %s ", column, bound.String(), width.String())
	}
	return b.String()
}

func eligibleSQL(cfg config.Universe) string {
	minTurnover, _ := cfg.MinTurnover.Float64()
	parts := []string{
		fmt.Sprintf("segment IN (%s)", quoteList(cfg.Segments)),
		fmt.Sprintf("turnover_med >= %f", minTurnover),
	}
	if cfg.ExcludeCapTerciles > 0 {
		parts = append(parts, fmt.Sprintf("cap_tercile > %d", cfg.ExcludeCapTerciles))
	}
	if cfg.ExcludeEarningsPrev {
		parts = append(parts, "NOT earn_prev")
	}
	if cfg.ExcludeEarningsToday {
		parts = append(parts, "NOT disc_today")
	}
	if cfg.ExcludeMarginAlert {
		parts = append(parts, "NOT alert")
	}
	if cfg.ExcludeLoss {
		parts = append(parts, "NOT is_loss")
	}
	return strings.Join(parts, " AND ")
}

func shortEligibleSQL(m config.Margin) string {
	if !m.Enabled {
		return "false"
	}
	minTurnover, _ := m.MinTurnover.Float64()
	parts := []string{
		"shortable",
		fmt.Sprintf("segment IN (%s)", quoteList(m.Segments)),
		fmt.Sprintf("turnover_med >= %f", minTurnover),
	}
	if m.ExcludeCapTerciles > 0 {
		parts = append(parts, fmt.Sprintf("cap_tercile > %d", m.ExcludeCapTerciles))
	}
	if m.ExcludeEarningsPrev {
		parts = append(parts, "NOT earn_prev")
	}
	if m.ExcludeEarningsToday {
		parts = append(parts, "NOT disc_today")
	}
	if m.ExcludeMarginAlert {
		parts = append(parts, "NOT alert")
	}
	if m.ExcludeJsfStop {
		parts = append(parts, "NOT jsf_stop")
	}
	return strings.Join(parts, " AND ")
}

func quoteList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, "'"+strings.ReplaceAll(v, "'", "''")+"'")
	}
	return strings.Join(quoted, ", ")
}
