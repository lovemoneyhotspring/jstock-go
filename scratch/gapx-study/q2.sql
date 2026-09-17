WITH c AS (SELECT *, CAST(d AS DATE) dd FROM '/tmp/gapx/cand.parquet'),
s AS (SELECT *, row_number() OVER (PARTITION BY dd, sector ORDER BY key, code) sr FROM c),
t AS (SELECT *, row_number() OVER (PARTITION BY dd ORDER BY key, code) pr FROM s WHERE sr = 1),
pairs AS (
  SELECT a.dd, a.sector, a.net n1, b.net n2, a.gap g1, b.gap g2, a.vol20 v1, b.vol20 v2, a.key k1, b.key k2,
         a.resid r1, b.resid r2, a.prev_close p1, b.prev_close p2
  FROM t a JOIN s b ON b.dd = a.dd AND b.sector = a.sector AND b.sr = 2
  WHERE a.pr <= 3
)
SELECT CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per, count(*) n,
  round(avg(CASE WHEN sign(n1) <> sign(n2) THEN 1.0 ELSE 0 END),3) split_share,
  round(avg(CASE WHEN n2 > n1 THEN 1.0 ELSE 0 END),3) second_wins,
  round(avg(n1),1) first_bp, round(avg(n2),1) second_bp, round(avg(n2-n1),2) diff,
  round(avg(n2-n1)/(stddev(n2-n1)/sqrt(count(*))),2) t,
  round(corr(n1, n2),2) corr12
FROM pairs GROUP BY ROLLUP(per) ORDER BY per NULLS LAST
