COPY (
WITH p AS (
  SELECT CAST(d AS DATE) dd, code, o, c, prev_close FROM read_csv('/tmp/minute/panel10.csv', types={'code':'VARCHAR'})
  WHERE o > 0 AND c > 0 AND prev_close > 0
),
m AS (
  SELECT CAST("Date" AS DATE) dd, CAST("Code" AS VARCHAR) code, CAST("S33" AS VARCHAR) sector
  FROM read_parquet('data/jquants/equities_master/*.parquet', union_by_name=true) WHERE "Date" >= DATE '2016-11-01'
),
va AS (
  SELECT m.dd, m.sector, sum(TRY_CAST(b."Va" AS DOUBLE)) va
  FROM read_parquet('data/jquants/equities_bars_daily/*.parquet', union_by_name=true) b
  JOIN m ON m.dd = CAST(b."Date" AS DATE) AND m.code = CAST(b."Code" AS VARCHAR)
  WHERE b."Date" >= DATE '2016-11-01' GROUP BY 1, 2
),
s AS (
  SELECT p.dd, m.sector, median(c / o - 1) s_oc, median(c / prev_close - 1) s_cc, median(o / prev_close - 1) s_gap, count(*) n
  FROM p JOIN m USING (dd, code) GROUP BY 1, 2 HAVING count(*) >= 5
),
sv AS (SELECT s.*, va.va FROM s LEFT JOIN va USING (dd, sector)),
w AS (
  SELECT *,
    lag(s_cc) OVER x f_cc1,
    lag(s_oc) OVER x f_oc1,
    sum(s_cc) OVER (x ROWS BETWEEN 5 PRECEDING AND 1 PRECEDING) f_cc5,
    sum(s_cc) OVER (x ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) f_cc20,
    lag(va) OVER x / median(va) OVER (x ROWS BETWEEN 21 PRECEDING AND 2 PRECEDING) f_rva,
    stddev(s_cc) OVER (x ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) f_vol,
    s_gap f_gap,
    -- 連続下落日数: 前日までで最後に s_cc >= 0 だった位置からの距離
    row_number() OVER x AS rn,
    max(CASE WHEN s_cc >= 0 THEN 1 END) OVER x AS dummy
  FROM sv WINDOW x AS (PARTITION BY sector ORDER BY dd)
),
w2 AS (
  SELECT *, rn - 1 - coalesce(max(CASE WHEN s_cc >= 0 THEN rn END) OVER (PARTITION BY sector ORDER BY dd ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) f_down
  FROM w
)
SELECT dd, sector, n, s_oc, s_cc, s_gap, f_cc1, f_oc1, f_cc5, f_cc20, f_down, f_rva, f_vol, f_gap FROM w2 WHERE dd >= DATE '2017-02-01'
) TO '/tmp/gapx/sector_daily.parquet' (FORMAT parquet)
