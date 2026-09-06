-- 候補パネル（scratch/minute -panel-only の CSV）に、前日引けまでの情報で作る特徴量を付けて parquet に保存する。
-- 特徴量はすべて (code, 前営業日) の足で計算し、next_d（翌営業日）で当日の行に結合する＝先読みなし。
COPY (
WITH panel AS (
  SELECT CAST(d AS DATE) d, CAST(code AS VARCHAR) code, o, c, prev_close, next_open, vol20, gap, limit_low, limit_high, eligible, short_eligible
  FROM read_csv('/tmp/minute/panel10.csv', header = true, types = {'code': 'VARCHAR'})
),
codes AS (SELECT DISTINCT code FROM panel),
bars AS (
  SELECT CAST(b."Date" AS DATE) d, CAST(b."Code" AS VARCHAR) code,
         TRY_CAST(b."O" AS DOUBLE) o, TRY_CAST(b."H" AS DOUBLE) h, TRY_CAST(b."L" AS DOUBLE) l, TRY_CAST(b."C" AS DOUBLE) c,
         TRY_CAST(b."Va" AS DOUBLE) va, TRY_CAST(b."Vo" AS DOUBLE) vo, TRY_CAST(b."MktCap" AS DOUBLE) cap,
         coalesce(nullif(TRY_CAST(b."AdjFactor" AS DOUBLE), 0), 1) af
  FROM equities_bars_daily b JOIN codes USING (code)
  WHERE b."Date" >= DATE '2015-10-01' AND TRY_CAST(b."C" AS DOUBLE) > 0 AND TRY_CAST(b."O" AS DOUBLE) > 0
),
f1 AS (
  SELECT *, lead(d) OVER w next_d,
    c / (lag(c) OVER w * af) - 1 ret1,
    o / (lag(c) OVER w * af) - 1 gap_hist,
    c > o AS up_day, c < o AS down_day,
    c < lag(c) OVER w * af AS fell
  FROM bars WINDOW w AS (PARTITION BY code ORDER BY d)
),
f2 AS (
  SELECT *,
    c / lag(c, 5) OVER w - 1 ret5, c / lag(c, 20) OVER w - 1 ret20, c / lag(c, 60) OVER w - 1 ret60,
    c / max(h) OVER (w ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) - 1 dd20,
    c / max(h) OVER (w ROWS BETWEEN 59 PRECEDING AND CURRENT ROW) - 1 dd60,
    c / min(l) OVER (w ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) - 1 up20,
    va / nullif(median(va) OVER (w ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING), 0) rvol1,
    avg(va) OVER (w ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) va20,
    (h - l) / nullif(lag(c) OVER w, 0) range1,
    CASE WHEN h > l THEN (c - l) / (h - l) END close_pos1,
    c / avg(c) OVER (w ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) - 1 sma20_dist,
    c / avg(c) OVER (w ROWS BETWEEN 199 PRECEDING AND CURRENT ROW) - 1 sma200_dist,
    avg(CASE WHEN gap_hist < -0.02 THEN CASE WHEN up_day THEN 1.0 ELSE 0.0 END END) OVER (w ROWS BETWEEN 249 PRECEDING AND CURRENT ROW) fade_rate_long,
    count(CASE WHEN gap_hist < -0.02 THEN 1 END) OVER (w ROWS BETWEEN 249 PRECEDING AND CURRENT ROW) fade_n_long,
    avg(CASE WHEN gap_hist > 0.03 THEN CASE WHEN down_day THEN 1.0 ELSE 0.0 END END) OVER (w ROWS BETWEEN 249 PRECEDING AND CURRENT ROW) fade_rate_short,
    count(CASE WHEN gap_hist > 0.03 THEN 1 END) OVER (w ROWS BETWEEN 249 PRECEDING AND CURRENT ROW) fade_n_short,
    count(CASE WHEN abs(gap_hist) > 0.03 THEN 1 END) OVER (w ROWS BETWEEN 59 PRECEDING AND CURRENT ROW) jumpy60,
    avg(vo) OVER (w ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) vo20
  FROM f1 WINDOW w AS (PARTITION BY code ORDER BY d)
),
-- 連続陰線（前日終値比で下げた日）の日数: 直近の「上げた日」からの行数
f2b AS (SELECT *, row_number() OVER (PARTITION BY code ORDER BY d) rn FROM f2),
f3 AS (
  SELECT *,
    max(CASE WHEN NOT fell THEN rn END) OVER (PARTITION BY code ORDER BY d ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) last_up_rn
  FROM f2b
),
feat AS (
  SELECT code, next_d d, ret1, ret5, ret20, ret60, dd20, dd60, up20, rvol1, va20, range1, close_pos1, sma20_dist, sma200_dist,
    fade_rate_long, fade_n_long, fade_rate_short, fade_n_short, jumpy60, vo20, cap,
    rn - coalesce(last_up_rn, rn) AS cdown
  FROM f3 WHERE next_d IS NOT NULL
),
sector AS (
  SELECT code, S33 FROM (SELECT CAST("Code" AS VARCHAR) code, "S33" S33, row_number() OVER (PARTITION BY "Code" ORDER BY "Date" DESC) k FROM equities_master) WHERE k = 1
),
margin AS (
  SELECT CAST(m."Code" AS VARCHAR) code, CAST(m."Date" AS DATE) + INTERVAL 5 DAY avail,
         TRY_CAST(m."LongVol" AS DOUBLE) lv, TRY_CAST(m."ShrtVol" AS DOUBLE) sv
  FROM markets_margin_interest m
),
sr AS (
  SELECT CAST("Date" AS DATE) d, "S33" S33,
    (TRY_CAST("ShrtWithResVa" AS DOUBLE) + TRY_CAST("ShrtNoResVa" AS DOUBLE)) /
    nullif(TRY_CAST("SellExShortVa" AS DOUBLE) + TRY_CAST("ShrtWithResVa" AS DOUBLE) + TRY_CAST("ShrtNoResVa" AS DOUBLE), 0) ratio
  FROM markets_short_ratio
),
srz AS (
  SELECT d, S33, lead(d) OVER (PARTITION BY S33 ORDER BY d) next_d,
    (ratio - avg(ratio) OVER w) / nullif(stddev(ratio) OVER w, 0) z
  FROM sr WINDOW w AS (PARTITION BY S33 ORDER BY d ROWS BETWEEN 60 PRECEDING AND 1 PRECEDING)
),
daystat AS (
  SELECT d,
    median(CASE WHEN eligible THEN gap END) med_gap,
    avg(CASE WHEN eligible THEN CASE WHEN gap < -0.02 THEN 1.0 ELSE 0.0 END END) breadth_down,
    avg(CASE WHEN eligible THEN CASE WHEN gap > 0.03 THEN 1.0 ELSE 0.0 END END) breadth_up,
    count(*) FILTER (WHERE eligible AND gap < 0) n_long_pool,
    count(*) FILTER (WHERE short_eligible AND gap >= 0.05) n_short_pool
  FROM panel GROUP BY d
),
joined AS (
  SELECT p.*, f.* EXCLUDE (code, d), s.S33, ds.med_gap, ds.breadth_down, ds.breadth_up, ds.n_long_pool, ds.n_short_pool,
    z.z sector_short_z,
    mg.lv / nullif(mg.sv, 0) margin_ratio, mg.sv / nullif(f.vo20, 0) short_days, mg.lv / nullif(f.vo20, 0) long_days,
    dayofweek(p.d) dow,
    row_number() OVER (PARTITION BY p.d ORDER BY p.gap) gap_rank_long,
    row_number() OVER (PARTITION BY p.d ORDER BY p.gap DESC) gap_rank_short
  FROM panel p
  LEFT JOIN feat f ON f.code = p.code AND f.d = p.d
  LEFT JOIN sector s ON s.code = p.code
  LEFT JOIN daystat ds ON ds.d = p.d
  LEFT JOIN srz z ON z.S33 = s.S33 AND z.next_d = p.d
  ASOF LEFT JOIN margin mg ON mg.code = p.code AND mg.avail <= p.d
)
SELECT *,
  gap - med_gap rel_gap, gap / nullif(vol20, 0) gap_z,
  c / o - 1 long_ret_raw,
  CASE WHEN c <= limit_low AND next_open IS NOT NULL THEN next_open / o - 1 ELSE c / o - 1 END long_ret,
  CASE WHEN c >= limit_high AND next_open IS NOT NULL THEN o / next_open - 1 ELSE o / c - 1 END short_ret
FROM joined
) TO '/tmp/dtsel/features.parquet' (FORMAT PARQUET)
