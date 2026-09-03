package universe

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/archsql"
	"github.com/lovemoneyhotspring/jstock-go/pkg/daytrade/config"
	"github.com/lovemoneyhotspring/jstock-go/pkg/jquants/archive"
)

// 端点。名前で引くのは、定義（鍵・日付列）をアーカイブ側と 1 箇所に保つため。
var (
	EPBars         = archive.MustEndpoint("equities_bars_daily")
	EPMaster       = archive.MustEndpoint("equities_master")
	EPFins         = archive.MustEndpoint("fins_summary")
	EPEarningsDate = archive.MustEndpoint("fins_earnings_date")
	EPMarginAlert  = archive.MustEndpoint("markets_margin_alert")
	EPOptions225   = archive.MustEndpoint("derivatives_bars_daily_options_225")
	EPTopix        = archive.MustEndpoint("indices_bars_daily_topix")
)

// feature は 1 銘柄ぶんの、足から作った特徴量（前日までの情報だけ）。
type feature struct {
	code        string
	prevClose   float64
	turnoverMed float64
	mktCap      float64
	vol20       *float64
}

// Build は判定日 day の候補を作る。prevDay は前営業日。
//
// 足の集計（20 日中央値・20 日ボラ）は DuckDB に投げる（全銘柄 × 40 営業日の窓関数を
// Go で回すと遅く、式も読みにくい）。銘柄一覧と当日 1 日ぶんのフラグ（決算・規制）は
// 件数が小さいのでアーカイブから素直に読む。
func Build(arch *archive.Archive, day, prevDay time.Time, cfg config.Universe, margin config.Margin) ([]Candidate, error) {
	features, err := loadFeatures(arch, prevDay, cfg)
	if err != nil {
		return nil, err
	}
	if len(features) == 0 {
		return nil, fmt.Errorf("%s までの足がありません", prevDay.Format(archsql.DateLayout))
	}

	master, err := loadMaster(arch, day, prevDay)
	if err != nil {
		return nil, err
	}
	if len(master) == 0 {
		return nil, fmt.Errorf("銘柄一覧（equities/master）がありません。jquants sync を先に")
	}

	earnPrev, err := loadEarningsPrev(arch, prevDay)
	if err != nil {
		return nil, err
	}
	discToday, err := loadEarningsToday(arch, day)
	if err != nil {
		return nil, err
	}
	alert, jsfStop, err := loadMarginAlert(arch, prevDay)
	if err != nil {
		return nil, err
	}

	// 分位は「株式かつ流動性の下限を満たす全銘柄」で切る（研究と同じ）
	minTurnover, _ := cfg.MinTurnover.Float64()
	var rows []Candidate
	var caps []float64
	var mask []bool
	for _, f := range features {
		m, ok := master[f.code]
		if !ok || m.product != StockProduct {
			continue
		}
		rows = append(rows, Candidate{
			Code:        f.code,
			Symbol:      ToBrokerSymbol(f.code),
			Name:        m.name,
			Segment:     m.segment,
			PrevClose:   f.prevClose,
			TurnoverMed: f.turnoverMed,
			MktCap:      f.mktCap,
			Vol20:       f.vol20,
			EarnPrev:    earnPrev[f.code],
			DiscToday:   discToday[f.code],
			Alert:       alert[f.code],
			JsfStop:     jsfStop[f.code],
			Shortable:   m.shortable,
		})
		caps = append(caps, f.mktCap)
		mask = append(mask, f.turnoverMed >= minTurnover)
	}
	terciles := CapTerciles(caps, mask)
	for i := range rows {
		rows[i].CapTercile = terciles[i]
		rows[i].Eligible = Eligible(rows[i], cfg)
		rows[i].ShortEligible = ShortEligible(rows[i], margin)
	}
	sortStable(rows, func(a, b Candidate) bool { return a.Code < b.Code })
	return rows, nil
}

// loadFeatures は足から前日までの特徴量を作る。最終日が prevDay の銘柄だけ残す
// （上場廃止・売買停止で足が途切れた銘柄を翌朝の候補に入れないため）。
func loadFeatures(arch *archive.Archive, prevDay time.Time, cfg config.Universe) ([]feature, error) {
	lookback := prevDay.AddDate(0, 0, -(cfg.TurnoverDays*2 + 10))
	source, ok := archsql.Source(arch, EPBars, lookback, prevDay)
	if !ok {
		return nil, archsql.MissingError(EPBars)
	}
	db, err := archsql.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// TRY_CAST なのは保存形が全列文字列で、欠測が空文字で入りうるため。
	query := fmt.Sprintf(`
WITH bars AS (
  SELECT "Date" AS d, CAST("Code" AS VARCHAR) AS code,
         TRY_CAST("C" AS DOUBLE) AS c, TRY_CAST("Va" AS DOUBLE) AS va,
         TRY_CAST("MktCap" AS DOUBLE) AS cap
  FROM %s
  WHERE "Date" <= %s AND TRY_CAST("C" AS DOUBLE) > 0
),
ranked AS (
  SELECT *, c / lag(c) OVER (PARTITION BY code ORDER BY d) - 1 AS ret,
         row_number() OVER (PARTITION BY code ORDER BY d DESC) AS rn
  FROM bars
)
SELECT code,
       max(d) AS last_date,
       arg_max(c, d) AS prev_close,
       median(va) FILTER (WHERE rn <= %d) AS turnover_med,
       arg_max(cap, d) AS mkt_cap,
       stddev_samp(ret) FILTER (WHERE rn <= %d) AS vol20
FROM ranked
GROUP BY code`, source, archsql.Lit(prevDay), cfg.TurnoverDays, VolDays)

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("足の集計に失敗しました: %w", err)
	}
	defer rows.Close()

	var out []feature
	latest := time.Time{}
	var all []struct {
		f    feature
		last time.Time
	}
	for rows.Next() {
		var (
			code                string
			lastDate            time.Time
			prevClose, turnover sql.NullFloat64
			mktCap, vol20       sql.NullFloat64
		)
		if err := rows.Scan(&code, &lastDate, &prevClose, &turnover, &mktCap, &vol20); err != nil {
			return nil, err
		}
		if lastDate.After(latest) {
			latest = lastDate
		}
		f := feature{code: code, prevClose: prevClose.Float64, turnoverMed: turnover.Float64, mktCap: mktCap.Float64}
		if vol20.Valid {
			v := vol20.Float64
			f.vol20 = &v
		}
		all = append(all, struct {
			f    feature
			last time.Time
		}{f, lastDate})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	if !sameDay(latest, prevDay) {
		return nil, fmt.Errorf("前営業日 %s の足がありません（最新 %s）。jquants sync を先に",
			prevDay.Format(archsql.DateLayout), latest.Format(archsql.DateLayout))
	}
	for _, a := range all {
		if sameDay(a.last, prevDay) && a.f.prevClose > 0 {
			out = append(out, a.f)
		}
	}
	return out, nil
}

type masterRow struct {
	name      string
	segment   string
	product   string
	shortable bool
}

// loadMaster は判定日以前の最新 1 日ぶんの銘柄一覧。
func loadMaster(arch *archive.Archive, day, prevDay time.Time) (map[string]masterRow, error) {
	frame, err := arch.Read(EPMaster, prevDay.AddDate(0, 0, -10), day)
	if err != nil || frame == nil {
		return nil, err
	}
	latest := ""
	for i := range frame.Rows {
		d := text(frame.Get(i, "Date"))
		if d != "" && d <= day.Format(archsql.DateLayout) && d > latest {
			latest = d
		}
	}
	if latest == "" {
		return nil, nil
	}
	out := make(map[string]masterRow, frame.Height())
	for i := range frame.Rows {
		if text(frame.Get(i, "Date")) != latest {
			continue
		}
		code := text(frame.Get(i, "Code"))
		if code == "" {
			continue
		}
		out[code] = masterRow{
			name:      text(frame.Get(i, "CoName")),
			segment:   SegmentOf(text(frame.Get(i, "MktNm"))),
			product:   text(frame.Get(i, "ProdCat")),
			shortable: IsShortable(text(frame.Get(i, "Mrgn"))),
		}
	}
	return out, nil
}

// loadEarningsPrev は前営業日の**引け後**に決算短信を開示した銘柄。
func loadEarningsPrev(arch *archive.Archive, prevDay time.Time) (map[string]bool, error) {
	frame, err := arch.Read(EPFins, prevDay, prevDay)
	if err != nil || frame == nil {
		return map[string]bool{}, err
	}
	out := map[string]bool{}
	for i := range frame.Rows {
		if text(frame.Get(i, "DiscDate")) != prevDay.Format(archsql.DateLayout) {
			continue
		}
		if !IsPostClose(prevDay, text(frame.Get(i, "DiscTime"))) {
			continue
		}
		if code := text(frame.Get(i, "Code")); code != "" {
			out[code] = true
		}
	}
	return out, nil
}

// loadEarningsToday は当日に決算発表の予定がある銘柄（予定が前日までに出ているもの）。
func loadEarningsToday(arch *archive.Archive, day time.Time) (map[string]bool, error) {
	frame, err := arch.Read(EPEarningsDate, day.AddDate(0, 0, -120), day)
	if err != nil || frame == nil {
		return map[string]bool{}, err
	}
	target := day.Format(archsql.DateLayout)
	out := map[string]bool{}
	for i := range frame.Rows {
		if text(frame.Get(i, "SchDate")) != target {
			continue
		}
		// 予定が当日に出たもの（場中の追加公表）は前夜の判断材料にならない
		if pub := text(frame.Get(i, "PubDate")); pub == "" || pub >= target {
			continue
		}
		if code := text(frame.Get(i, "Code")); code != "" {
			out[code] = true
		}
	}
	return out, nil
}

// loadMarginAlert は前営業日に信用規制で公表された銘柄と、そのうち売り禁のもの。
func loadMarginAlert(arch *archive.Archive, prevDay time.Time) (alert, jsfStop map[string]bool, err error) {
	alert, jsfStop = map[string]bool{}, map[string]bool{}
	frame, err := arch.Read(EPMarginAlert, prevDay, prevDay)
	if err != nil || frame == nil {
		return alert, jsfStop, err
	}
	for i := range frame.Rows {
		code := text(frame.Get(i, "Code"))
		if code == "" {
			continue
		}
		alert[code] = true
		if IsJsfStop(text(frame.Get(i, "PubReason"))) {
			jsfStop[code] = true
		}
	}
	return alert, jsfStop, nil
}

func text(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}
