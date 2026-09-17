-- 採用（上位 3）のうち負けた取引の共通点。特徴量ごとに 5 分位（IS の閾値）の負け率と、負け − 勝ちの平均差の t を出す
WITH c AS (SELECT x.*, CAST(x.d AS DATE) dd FROM '/tmp/gapx/cand.parquet' x),
s AS (SELECT *, row_number() OVER (PARTITION BY dd, sector ORDER BY key, code) sr FROM c),
t AS (SELECT *, row_number() OVER (PARTITION BY dd ORDER BY key, code) pr FROM s WHERE sr = 1),
pk AS (SELECT * FROM t WHERE pr <= 3),
codes AS (SELECT DISTINCT code FROM pk),
bars AS (
  SELECT CAST(b."Date" AS DATE) bd, CAST(b."Code" AS VARCHAR) code, TRY_CAST(b."AdjC" AS DOUBLE) ac, TRY_CAST(b."H" AS DOUBLE) h, TRY_CAST(b."L" AS DOUBLE) l, TRY_CAST(b."C" AS DOUBLE) cl, TRY_CAST(b."O" AS DOUBLE) op,
         TRY_CAST(b."Va" AS DOUBLE) va, TRY_CAST(b."MktCap" AS DOUBLE) mcap
  FROM read_parquet('data/jquants/equities_bars_daily/*.parquet', union_by_name=true) b
  WHERE CAST(b."Code" AS VARCHAR) IN (SELECT code FROM codes) AND b."Date" >= DATE '2016-09-01'
),
bw AS (
  SELECT *, lead(bd) OVER w nd,
    ac / lag(ac, 1) OVER w - 1 r1, ac / lag(ac, 5) OVER w - 1 r5, ac / lag(ac, 20) OVER w - 1 r20,
    (h - l) / nullif(cl, 0) rng, (cl - l) / nullif(h - l, 0) cpos,
    va / nullif(median(va) OVER (w ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING), 0) rva,
    ac / avg(ac) OVER (w ROWS BETWEEN 24 PRECEDING AND CURRENT ROW) - 1 sma25
  FROM bars WINDOW w AS (PARTITION BY code ORDER BY bd)
),
today AS (SELECT bd, code, h / op - 1 up_max, l / op - 1 dn_max FROM bars),
m AS (SELECT CAST("Date" AS DATE) md, CAST("Code" AS VARCHAR) code, CAST("ScaleCat" AS VARCHAR) scale FROM read_parquet('data/jquants/equities_master/*.parquet', union_by_name=true) WHERE "Date" >= DATE '2017-01-01'),
tpx AS (SELECT CAST("Date" AS DATE) td, (TRY_CAST("C" AS DOUBLE) / TRY_CAST("O" AS DOUBLE) - 1) * 1e4 tpx_oc FROM read_parquet('data/jquants/indices_bars_daily_topix/*.parquet', union_by_name=true)),
secday AS (SELECT dd, sector, median((c / o - 1) * 1e4) sec_oc FROM c GROUP BY 1, 2),
us AS (SELECT CAST(date AS DATE) ud, spx / lag(spx) OVER (ORDER BY date) - 1 spx, vix FROM read_json('data/daytrade/us.json')),
f0 AS (
  SELECT pk.dd, pk.code, pk.net, pk.pr, pk.gap, pk.vol20, pk.key, pk.prev_close, pk.sector_gap,
    bw.r1, bw.r5, bw.r20, bw.rng, bw.cpos, bw.rva, bw.sma25, ln(bw.mcap) lnmcap,
    m.scale, isodow(pk.dd) dow, tpx.tpx_oc, sd.sec_oc, td.up_max, td.dn_max
  FROM pk
  LEFT JOIN bw ON bw.code = pk.code AND bw.nd = pk.dd
  LEFT JOIN m ON m.code = pk.code AND m.md = pk.dd
  LEFT JOIN tpx ON tpx.td = pk.dd
  LEFT JOIN secday sd ON sd.dd = pk.dd AND sd.sector = pk.sector
  LEFT JOIN today td ON td.code = pk.code AND td.bd = pk.dd
),
f AS (
  SELECT f0.*, us.spx, us.vix, CASE WHEN net < 0 THEN 1.0 ELSE 0 END lose,
         CASE WHEN f0.dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per
  FROM f0 ASOF LEFT JOIN us ON f0.dd > us.ud
)
SELECT * FROM f
