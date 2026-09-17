WITH c AS (SELECT *, CAST(d AS DATE) dd FROM '/tmp/gapx/cand.parquet'),
pick AS (
  -- 本番: key、V1: rkey、V2: 説明された候補を除いて key
  SELECT 'base' v, dd, net, key AS k, sector, code FROM c
  UNION ALL SELECT 'V1', dd, net, rkey, sector, code FROM c
  UNION ALL SELECT 'V2', dd, net, key, sector, code FROM c
    WHERE NOT (sector_gap IS NOT NULL AND sector_gap / gap >= 0.5)
),
s AS (SELECT *, row_number() OVER (PARTITION BY v, dd, sector ORDER BY k, code) sr FROM pick),
t AS (SELECT *, row_number() OVER (PARTITION BY v, dd ORDER BY k, code) pr FROM s WHERE sr = 1),
daily AS (SELECT v, dd, avg(net) m FROM t WHERE pr <= 3 GROUP BY v, dd),
w AS (
  SELECT b.dd, x.v, x.m - b.m AS diff, b.m AS bm, x.m AS xm
  FROM daily b JOIN daily x ON x.dd = b.dd AND x.v <> 'base' WHERE b.v = 'base'
)
SELECT v, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per,
  count(*) n, round(avg(bm),2) base_bp, round(avg(xm),2) var_bp, round(avg(diff),2) diff,
  round(avg(diff)/(stddev(diff)/sqrt(count(*))),2) t,
  round(avg(CASE WHEN diff <> 0 THEN 1.0 ELSE 0 END),3) changed_days
FROM w GROUP BY GROUPING SETS ((v, per), (v)) ORDER BY v, per NULLS LAST
