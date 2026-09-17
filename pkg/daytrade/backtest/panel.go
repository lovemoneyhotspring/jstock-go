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
	"strconv"
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
	// Ret1 ほかは並べ替えの機械学習の特徴量（universe.Candidate と同じ定義。無ければ nil）。
	Ret1, Ret5, Ret20, Pos20, PrevIntraday *float64
	// ShortInterest は空売り残高（発行済に対する比。報告が無ければ nil）。
	// ショートの母集団の条件（margin.max_short_interest）に使う。
	ShortInterest *float64
	// ここから下は母集団の判定（universe.Eligible / ShortEligible）の材料。
	// 判定そのものは SQL ではなく Go で当てる——前夜の plan と同じ関数にするため。
	Segment     string
	Shortable   bool
	TurnoverMed float64
	// MktCap は前日の時価総額（百万円）。時価総額の 3 分位を出すのに使う。
	MktCap     float64
	CapTercile int
	EarnPrev   bool
	DiscToday  bool
	Alert      bool
	JsfStop    bool
	Loss       bool
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

// applyUniverse は 1 日ぶんの行に母集団の判定を当て、ロング・ショートどちらかに
// 入る行だけを返す。判定は前夜の plan と同じ関数（universe）。
//
// 時価総額の 3 分位もここで出す（母数は売買代金が min_turnover 以上の銘柄。
// 分割・併合の日の行も母数には入れてから捨てる——前夜の plan と同じ母数にするため）。
func applyUniverse(day []Row, cfg config.Config, terciles []int) []Row {
	if terciles == nil {
		terciles = tercilesOf(day, cfg)
	}
	long, short := universe.NewFilter(cfg.Universe), universe.NewShortFilter(cfg.Margin)
	out := make([]Row, 0, len(day))
	for i, r := range day {
		if r.PrevClose <= 0 {
			continue
		}
		r.CapTercile = terciles[i]
		c := candidateOf(r)
		r.Eligible = long.Match(c)
		r.ShortEligible = short.Match(c)
		// どちらにも入らない行は持たない（全銘柄 × 10 年はメモリを食うだけ）
		if !r.Eligible && !r.ShortEligible {
			continue
		}
		out = append(out, r)
	}
	return out
}

// tercilesOf は 1 日ぶんの時価総額の 3 分位（母数は売買代金が min_turnover 以上の銘柄）。
func tercilesOf(day []Row, cfg config.Config) []int {
	minTurnover, _ := cfg.Universe.MinTurnover.Float64()
	caps := make([]float64, len(day))
	mask := make([]bool, len(day))
	for i, r := range day {
		caps[i], mask[i] = r.MktCap, r.TurnoverMed >= minTurnover
	}
	return universe.CapTerciles(caps, mask)
}

// UniverseView は KeepAll で読んだパネルに 1 つの設定の母集団を当てた見え方を返す。
// 元のパネルは変えない（格子は同じパネルを設定の数だけ当て直す）。
//
// 行は日ごとにまとまっている前提（LoadPanel は d, code 順に並べて返す）。
func UniverseView(panel *Panel, cfg config.Config) *Panel {
	return universeViewWith(panel, cfg, nil, 0)
}

// universeViewWith は 3 分位を外から渡せる UniverseView。terciles は全行ぶんを
// 行の並びのまま並べたもの（min_turnover が同じ設定どうしで使い回す——格子で
// 設定ごとに 490 万行を並べ替え直すと 1 本あたり数秒を無駄に払う）。
// hint は結果の行数の見当（0 なら見当なし）。
func universeViewWith(panel *Panel, cfg config.Config, terciles []int, hint int) *Panel {
	out := &Panel{Days: panel.Days, Rows: make([]Row, 0, hint)}
	eachDay(panel.Rows, func(from, to int) {
		var t []int
		if terciles != nil {
			t = terciles[from:to]
		}
		out.Rows = append(out.Rows, applyUniverse(panel.Rows[from:to], cfg, t)...)
	})
	return out
}

// tercilesFor はパネル全体ぶんの 3 分位（行の並びのまま）。
func tercilesFor(panel *Panel, cfg config.Config) []int {
	out := make([]int, 0, len(panel.Rows))
	eachDay(panel.Rows, func(from, to int) {
		out = append(out, tercilesOf(panel.Rows[from:to], cfg)...)
	})
	return out
}

// eachDay は日ごとの区切り [from, to) を順に渡す（行は日ごとにまとまっている前提）。
func eachDay(rows []Row, fn func(from, to int)) {
	from := 0
	for i := 1; i <= len(rows); i++ {
		if i < len(rows) && rows[i].Date.Equal(rows[from].Date) {
			continue
		}
		fn(from, i)
		from = i
	}
}

// floorOf はパネルに残す売買代金の下限。
//
// キャッシュは設定に依存しない下限（PanelTurnoverFloor）で作ってあるが、**読み出しは
// その設定の min_turnover まで上げられる**——それ未満の行はロングにもショートにも入らず、
// 時価総額の 3 分位の母数（turnover_med >= universe.min_turnover）にも入らないので、
// 落としても結果は 1 円も変わらない。Scan する行が 490 → 341 万行に減る。
func floorOf(popts PanelOptions, cfg config.Config) float64 {
	if popts.TurnoverFloor > 0 {
		return popts.TurnoverFloor
	}
	floor, _ := cfg.Universe.MinTurnover.Float64()
	if cfg.Margin.Enabled {
		if m, _ := cfg.Margin.MinTurnover.Float64(); m < floor {
			floor = m
		}
	}
	if floor < PanelTurnoverFloor {
		return PanelTurnoverFloor
	}
	return floor
}

// panelSelectColumns はパネルの列。**設定に依存しない特徴量だけ**を返し、
// 母集団の判定（eligible / short_eligible）と時価総額の 3 分位は Go 側で当てる
// ——前夜の plan と同じ関数（universe）を使うため。
const panelSelectColumns = `d, code, o, c, prev_close, next_open, vol20, earn_yield, sector,
       segment, shortable, turnover_med, mkt_cap, earn_prev, disc_today, alert, jsf_stop, is_loss,
       short_interest, ret1, ret5, ret20, pos20, prev_intraday`

// LoadPanel は (Date, Code) ごとの特徴量と当日の寄付・終値を作る。
// eligible / short_eligible のどちらかに入る行だけを返す（全銘柄 × 10 年を持つと
// メモリを食うだけで、判断に使わない）。
func LoadPanel(arch *archive.Archive, start, end time.Time, cfg config.Config) (*Panel, error) {
	return LoadPanelWith(arch, start, end, cfg, PanelOptions{})
}

// PanelOptions はパネルの読み方。ゼロ値は「その設定の母集団だけを持つ」。
type PanelOptions struct {
	// KeepAll が真なら母集団の判定を当てず、下限を満たす行を全部返す。
	// 格子（backtest --grid）が設定ごとに UniverseView で当て直すための形。
	KeepAll bool
	// TurnoverFloor は残す売買代金 20 日中央値の下限（円）。0 なら PanelTurnoverFloor。
	// 格子では「並べた設定の min_turnover の最小値」を渡して行数を抑える。
	TurnoverFloor float64
}

// LoadPanelWith は読み方を指定して LoadPanel する。
func LoadPanelWith(arch *archive.Archive, start, end time.Time, cfg config.Config, popts PanelOptions) (*Panel, error) {
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
	ssrSrc, hasSSR := archsql.Source(arch, universe.EPShortSale, start.AddDate(0, 0, -180), end)

	query := buildPanelQuery(panelSources{
		bars: barsSrc, master: masterSrc,
		fins: finsSrc, hasFins: hasFins,
		sched: schedSrc, hasSched: hasSched,
		alert: alertSrc, hasAlert: hasAlert,
		ssr: ssrSrc, hasSSR: hasSSR,
	}, start, end, cfg, floorOf(popts, cfg))

	// キャッシュがあれば（作れれば）そちらから読む。作れなくても検証は続ける
	// ——遅くなるだけで結果は同じなので、ここで止める理由が無い。
	queries := []string{query}
	if PanelCacheEnabled {
		if path, err := ensurePanelCache(db, arch, cfg, end); err != nil {
			fmt.Fprintf(os.Stderr, "パネルのキャッシュを使えません（そのまま実行します）: %v\n", err)
		} else {
			queries = cachedPanelQueries(path, start, end, floorOf(popts, cfg))
		}
	}

	panel := &Panel{}
	// 同じ日の行をためて、その日の母集団を前夜の plan と同じ関数で決める。
	var day []Row
	flush := func() {
		if len(day) == 0 {
			return
		}
		if popts.KeepAll {
			// 分割・併合の日（prev_close が無い）の行も残す——3 分位の母数に入るので、
			// ここで捨てると設定ごとに当て直したときの分位が単独実行とずれる。
			// 建てる対象から外すのは applyUniverse。
			panel.Rows = append(panel.Rows, day...)
		} else {
			panel.Rows = append(panel.Rows, applyUniverse(day, cfg, nil)...)
		}
		day = day[:0]
	}
	scan := func(rows *sql.Rows) error {
		for rows.Next() {
			var (
				r                 Row
				prevClose         sql.NullFloat64
				nextOpen, vol20   sql.NullFloat64
				earnYield         sql.NullFloat64
				sector, segment   sql.NullString
				mktCap            sql.NullFloat64
				shortInterest     sql.NullFloat64
				ret1, ret5, ret20 sql.NullFloat64
				pos20, prevIntra  sql.NullFloat64
				gap               sql.NullFloat64
				limitLow, limitHi sql.NullFloat64
			)
			if err := rows.Scan(&r.Date, &r.Code, &r.Open, &r.Close, &prevClose,
				&nextOpen, &vol20, &earnYield, &sector, &segment, &r.Shortable,
				&r.TurnoverMed, &mktCap, &r.EarnPrev, &r.DiscToday, &r.Alert, &r.JsfStop, &r.Loss,
				&shortInterest, &ret1, &ret5, &ret20, &pos20, &prevIntra,
				&gap, &limitLow, &limitHi); err != nil {
				return err
			}
			r.Date = r.Date.UTC()
			if len(day) > 0 && !r.Date.Equal(day[0].Date) {
				flush()
			}
			r.PrevClose, r.MktCap, r.Gap = prevClose.Float64, mktCap.Float64, gap.Float64
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
			r.ShortInterest = nullable(shortInterest)
			r.Ret1, r.Ret5, r.Ret20 = nullable(ret1), nullable(ret5), nullable(ret20)
			r.Pos20, r.PrevIntraday = nullable(pos20), nullable(prevIntra)
			r.Sector, r.Segment = sector.String, segment.String
			r.LimitLow, r.LimitHigh = limitLow.Float64, limitHi.Float64
			day = append(day, r)
		}
		return rows.Err()
	}
	// 読み出しは 1 年ずつ（cachedPanelQueries）。日ごとの判定は 1 日で閉じている。
	for _, q := range queries {
		rows, err := db.Query(q)
		if err != nil {
			return nil, fmt.Errorf("パネルの組み立てに失敗しました: %w", err)
		}
		err = scan(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		flush()
	}

	days, err := tradingDays(db, barsSrc, start, end)
	if err != nil {
		return nil, err
	}
	// 上限を掛ける設定なのに残高が 1 件も入っていなければ、判定は黙って素通りする
	// （2026-09-12: キャッシュ側の経路に端点を足し忘れて、全行 NULL のまま通っていた）。
	if cfg.Margin.MaxShortInterest.IsPositive() {
		withSI := 0
		for i := range panel.Rows {
			if panel.Rows[i].ShortInterest != nil {
				withSI++
			}
		}
		if withSI == 0 {
			return nil, fmt.Errorf("margin.max_short_interest を掛ける設定ですが、空売り残高が 1 件も取れていません（markets_short_sale_report の取り込みを確認してください）")
		}
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
	ssr          string
	hasSSR       bool
}

// buildPanelQuery はパネルの SQL を組み立てる。
//
// 決算・規制のフラグは「前営業日に起きたことが翌営業日に効く」ので、日付そのものでは
// なく営業日の連番（di）で 1 日ずらす。暦日で足すと連休明けに効かなくなる。
func buildPanelQuery(src panelSources, start, end time.Time, cfg config.Config, floor float64) string {
	var b strings.Builder
	b.WriteString(panelCTEs(src, start, end, cfg))
	fmt.Fprintf(&b, `SELECT %s,
       o / prev_close - 1 AS gap,
       prev_close - (%s) AS limit_low,
       prev_close + (%s) AS limit_high
FROM valued
WHERE turnover_med >= %f
ORDER BY d, code`,
		panelSelectColumns, limitWidthSQL("prev_close"), limitWidthSQL("prev_close"), floor)
	return b.String()
}

// panelCTEs は WITH 句（bars … valued）。直接の経路とキャッシュ作成で共有する。
// start は joined の絞り込みにだけ効くので、キャッシュ作成では最古を渡す。
func panelCTEs(src panelSources, start, end time.Time, cfg config.Config) string {
	var b strings.Builder
	// {{pos_days}} は universe.PosDays（書式の番号付き引数と混ぜると後ろの %d がずれるので置換で埋める）
	b.WriteString(strings.ReplaceAll(fmt.Sprintf(`
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
         lag(b.cap) OVER w AS mkt_cap,
         -- 並べ替えの機械学習の特徴量の材料。z は係数で揃えた終値（universe.loadFeatures と同じ）
         b.c / exp(sum(ln(b.af)) OVER (w ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)) AS z,
         lag(b.c) OVER w / nullif(lag(b.o) OVER w, 0) - 1 AS prev_intraday
  FROM bars b
  WINDOW w AS (PARTITION BY b.code ORDER BY b.d)
),
rolled AS (
  SELECT l.*,
         CASE WHEN count(l.va) OVER win = %d THEN median(l.va) OVER win END AS turnover_med,
         CASE WHEN count(l.ret) OVER vwin = %d THEN stddev_samp(l.ret) OVER vwin END AS vol20,
         lag(l.z, 1) OVER w / lag(l.z, 2) OVER w - 1 AS ret1,
         lag(l.z, 1) OVER w / lag(l.z, 5) OVER w - 1 AS ret5,
         lag(l.z, 1) OVER w / lag(l.z, {{pos_days}}) OVER w - 1 AS ret20,
         CASE WHEN count(l.z) OVER pwin = {{pos_days}}
              THEN (lag(l.z, 1) OVER w - min(l.z) OVER pwin) / nullif(max(l.z) OVER pwin - min(l.z) OVER pwin, 0) END AS pos20
  FROM lagged l
  WINDOW win AS (PARTITION BY l.code ORDER BY l.d ROWS BETWEEN %d PRECEDING AND 1 PRECEDING),
         vwin AS (PARTITION BY l.code ORDER BY l.d ROWS BETWEEN %d PRECEDING AND 1 PRECEDING),
         pwin AS (PARTITION BY l.code ORDER BY l.d ROWS BETWEEN {{pos_days}} PRECEDING AND 1 PRECEDING),
         w AS (PARTITION BY l.code ORDER BY l.d)
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
		archsql.Lit(start), universe.StockProduct), "{{pos_days}}", strconv.Itoa(universe.PosDays)))

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

	// 空売り残高（markets/short-sale-report）。**判定日より前に公表された**報告だけを使う
	// （前夜の plan が読めるのはそれだけ。universe.loadShortInterest と同じ意味）。
	// 公表日 dd ごとに「それまでに公表された最新の計算日 lc と、その計算日の報告の合計」を持ち、
	// 判定日より前の最新の dd を ASOF で当てる。lc の報告の公表が窓より古ければ無し。
	// 以前は計算日で当てていて、公表前（計算日の 2 営業日後に公表）の値を使っていた。
	if src.hasSSR {
		fmt.Fprintf(&b, `ssr_raw AS (
  SELECT CAST(s."Code" AS VARCHAR) AS code, s."CalcDate" AS cd, s."DiscDate" AS dd,
         TRY_CAST(s."ShrtPosToSO" AS DOUBLE) AS v
  FROM %s s WHERE s."CalcDate" IS NOT NULL AND s."DiscDate" IS NOT NULL
),
ssr_last AS (
  SELECT code, dd, max(max(cd)) OVER (PARTITION BY code ORDER BY dd ROWS UNBOUNDED PRECEDING) AS lc
  FROM ssr_raw GROUP BY code, dd
),
ssr AS (
  SELECT l.code, l.dd, sum(r.v) AS si, max(r.dd) AS ld
  FROM ssr_last l JOIN ssr_raw r ON r.code = l.code AND r.cd = l.lc AND r.dd <= l.dd
  GROUP BY l.code, l.dd
),
`, src.ssr)
	} else {
		b.WriteString("ssr AS (SELECT NULL::VARCHAR AS code, NULL::DATE AS dd, NULL::DOUBLE AS si, NULL::DATE AS ld WHERE false),\n")
	}

	fmt.Fprintf(&b, `flagged AS (
  SELECT j.*,
         coalesce(e.code IS NOT NULL, false) AS earn_prev,
         coalesce(s.code IS NOT NULL, false) AS disc_today,
         coalesce(al.code IS NOT NULL, false) AS alert,
         coalesce(al.jsf_stop, false) AS jsf_stop,
         CASE WHEN ss.ld >= j.d - INTERVAL %[2]d DAY THEN ss.si END AS short_interest
  FROM joined j
  LEFT JOIN earn e ON e.code = j.code AND e.di = j.di
  LEFT JOIN sched s ON s.code = j.code AND s.d = j.d
  LEFT JOIN alerts al ON al.code = j.code AND al.di = j.di
  ASOF LEFT JOIN ssr ss ON ss.code = j.code AND ss.dd < j.d
),
-- 本決算は「その日から %[1]d 日以内に開示されたもの」だけを見る。実運用の plan が
-- 判定日から遡って探すのと同じ窓にする（universe.FinsLookbackDays）。絶対の窓で切ると
-- --since によって同じ日の判定が変わり、キャッシュとも食い違う。
valued AS (
  SELECT t.*,
         np_fy / nullif(t.mkt_cap * 1e6, 0) AS earn_yield,
         coalesce(np_fy <= 0, false) AS is_loss
  FROM (
    SELECT t.*, CASE WHEN fy.fd >= t.d - INTERVAL %[1]d DAY THEN fy.np END AS np_fy
    FROM flagged t ASOF LEFT JOIN finsfy fy ON fy.code = t.code AND fy.fd < t.d
  ) t
)
`,
		universe.FinsLookbackDays, universe.ShortInterestLookbackDays)
	return b.String()
}

// panelCacheColumns はキャッシュに落とす列。設定に依存しないものだけを持ち、
// 設定に依存する判定（eligible / short_eligible・ギャップ・制限値幅）は読み出し側で当てる。
const panelCacheColumns = `SELECT d, code, o, c, prev_close, next_open, next_open_d, vol20, earn_yield,
       sector, segment, shortable, turnover_med, mkt_cap, earn_prev, disc_today, alert, jsf_stop, is_loss,
       short_interest, ret1, ret5, ret20, pos20, prev_intraday`

// buildCacheQuery はキャッシュに落とす行を作る SQL。期間は切らず（読み出し側で切る）、
// 流動性の下限だけで絞る——下限を満たさない行はどの設定でも母集団に入らないため。
// 下限は設定によらない固定値（PanelTurnoverFloor）。
//
// **prev_close が無い行（分割・併合の日）も残す**。建てる対象にはならないが、
// 時価総額の 3 分位の母数には入る（前夜の plan と同じ母数にするため）。読み出し側で捨てる。
func buildCacheQuery(src panelSources, end time.Time, cfg config.Config) string {
	var b strings.Builder
	b.WriteString(panelCTEs(src, time.Time{}, end, cfg))
	fmt.Fprintf(&b, `%s
FROM valued
WHERE turnover_med >= %f`, panelCacheColumns, PanelTurnoverFloor)
	return b.String()
}

// PanelTurnoverFloor はパネルに残す売買代金 20 日中央値の下限（円）。設定の
// min_turnover（両設定とも 1 億円）より低ければよく、設定に依存しない値にすることで
// パネルとそのキャッシュが設定に依存しなくなる（格子を 1 回の読み込みで回せる）。
const PanelTurnoverFloor = 5e7

// buildCachedPanelQuery はキャッシュから読む SQL。直接の経路と同じ行・同じ値を返す。
//
// next_open は「次の足が end より後なら NULL」にする。キャッシュは全期間で作るので、
// そうしないと期間の終わりに未来の足が見えてしまう（上場廃止・売買停止で end より前に
// 足が途切れる銘柄でも同じ。営業日で切ると取りこぼす）。
func buildCachedPanelQuery(cachePath string, start, end time.Time, floor float64) string {
	return fmt.Sprintf(`SELECT d, code, o, c, prev_close,
       CASE WHEN next_open_d > %[2]s THEN NULL ELSE next_open END AS next_open,
       vol20, earn_yield, sector, segment, shortable, turnover_med, mkt_cap,
       earn_prev, disc_today, alert, jsf_stop, is_loss, short_interest,
       ret1, ret5, ret20, pos20, prev_intraday,
       o / prev_close - 1 AS gap,
       prev_close - (%[3]s) AS limit_low,
       prev_close + (%[3]s) AS limit_high
FROM read_parquet(%[4]s)
WHERE d >= %[1]s AND d <= %[2]s AND turnover_med >= %[5]f
ORDER BY d, code`,
		archsql.Lit(start), archsql.Lit(end), limitWidthSQL("prev_close"),
		archsql.LitString(cachePath), floor)
}

// cachedPanelQueries はキャッシュから読む SQL を 1 年ずつに割ったもの。
//
// 日ごとの判定（3 分位）は 1 日で閉じているので、年で割っても結果は同じ。割るのは
// 並べ替えのピークを下げるため——10 年ぶん（490 万行）を 1 度に並べると 2.5GB 増える
// （2026-09-12 の実測。このマシンはメモリが制約側）。
func cachedPanelQueries(cachePath string, start, end time.Time, floor float64) []string {
	var out []string
	for from := start; !from.After(end); from = time.Date(from.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC) {
		to := time.Date(from.Year(), 12, 31, 0, 0, 0, 0, time.UTC)
		if to.After(end) {
			to = end
		}
		out = append(out, buildCachedPanelQuery(cachePath, from, to, floor))
	}
	return out
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

// nullable は NULL を nil にする。
func nullable(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	x := v.Float64
	return &x
}
