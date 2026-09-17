WITH sd AS (SELECT dd, sector, f_rva FROM '/tmp/gapx/sector_daily.parquet'),
c AS (
  SELECT x.*, CAST(x.d AS DATE) dd, sd.f_rva FROM '/tmp/gapx/cand.parquet' x
  LEFT JOIN sd ON sd.dd = CAST(x.d AS DATE) AND sd.sector = x.sector
),
th AS (SELECT quantile_cont(f_rva, 0.2) q20 FROM c WHERE dd < DATE '2022-01-01'),
v AS (
  SELECT 'base' v, dd, code, sector, net, key, f_rva FROM c
  UNION ALL
  SELECT 'R1', dd, code, sector, net, key, f_rva FROM c WHERE f_rva IS NULL OR f_rva > (SELECT q20 FROM th)
),
s AS (SELECT *, row_number() OVER (PARTITION BY v, dd, sector ORDER BY key, code) sr FROM v),
t AS (SELECT *, row_number() OVER (PARTITION BY v, dd ORDER BY key, code) pr FROM s WHERE sr = 1),
r2 AS (
  SELECT 'R2' v, dd, net, row_number() OVER (PARTITION BY dd ORDER BY f_rva DESC NULLS LAST, pr) pr
  FROM t WHERE v = 'base' AND pr <= 6
),
picks AS (SELECT v, dd, net FROM t WHERE pr <= 3 UNION ALL SELECT v, dd, net FROM r2 WHERE pr <= 3),
daily AS (SELECT v, dd, avg(net) m FROM picks GROUP BY 1, 2),
j AS (SELECT b.dd, x.v, b.m bm, x.m xm FROM daily b JOIN daily x ON x.dd = b.dd AND x.v <> 'base' WHERE b.v = 'base'),
stats AS (
  SELECT v, dd, m, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per,
         sum(m) OVER (PARTITION BY v ORDER BY dd) cum,
         sum(m) OVER (PARTITION BY v, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END ORDER BY dd) pcum
  FROM daily
),
dd AS (
  SELECT v, per, max(ppeak - pcum) mdd FROM (SELECT *, max(pcum) OVER (PARTITION BY v, per ORDER BY dd) ppeak FROM stats) GROUP BY v, per
  UNION ALL
  SELECT v, NULL, max(peak - cum) FROM (SELECT *, max(cum) OVER (PARTITION BY v ORDER BY dd) peak FROM stats) GROUP BY v
),
perf AS (SELECT v, per, count(*) n, avg(m) bp, avg(m) / stddev(m) * sqrt(245) sharpe, sum(m) total FROM stats GROUP BY ROLLUP(v, per)),
diff AS (
  SELECT v, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per, avg(xm - bm) d, avg(xm - bm) / (stddev(xm - bm) / sqrt(count(*))) t,
         avg(CASE WHEN xm <> bm THEN 1.0 ELSE 0 END) changed
  FROM j GROUP BY ROLLUP(v, per)
)
SELECT perf.v, perf.per, perf.n, round(perf.bp, 2) bp_day, round(perf.sharpe, 2) sharpe, round(perf.total) total_bp, round(dd.mdd) max_dd_bp,
       round(diff.d, 2) diff, round(diff.t, 2) t, round(diff.changed, 3) changed
FROM perf LEFT JOIN dd ON dd.v = perf.v AND dd.per IS NOT DISTINCT FROM perf.per
     LEFT JOIN diff ON diff.v = perf.v AND diff.per IS NOT DISTINCT FROM perf.per
WHERE perf.v IS NOT NULL ORDER BY perf.v, perf.per NULLS LAST
