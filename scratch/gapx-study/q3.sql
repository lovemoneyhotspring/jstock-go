WITH c AS (SELECT *, CAST(d AS DATE) dd FROM '/tmp/gapx/cand.parquet'),
s AS (SELECT *, row_number() OVER (PARTITION BY dd, sector ORDER BY key, code) sr,
             row_number() OVER (PARTITION BY dd ORDER BY key, code) ov FROM c),
t AS (SELECT dd, code, row_number() OVER (PARTITION BY dd ORDER BY key, code) pr FROM s WHERE sr = 1),
x AS (SELECT s.*, t.pr FROM s LEFT JOIN t USING (dd, code)),
cut AS (SELECT dd, max(ov) FILTER (WHERE pr = 3) ov3 FROM x GROUP BY dd),
g AS (
  SELECT x.dd,
    avg(net) FILTER (WHERE pr <= 3) pick,
    avg(net) FILTER (WHERE (sr >= 2 AND ov < ov3) OR pr BETWEEN 4 AND 6) runner,
    avg(net) FILTER (WHERE sr >= 2 AND ov < ov3) capd,
    avg(net) FILTER (WHERE pr BETWEEN 4 AND 6) nextn
  FROM x JOIN cut USING (dd) WHERE cut.ov3 IS NOT NULL GROUP BY x.dd
)
SELECT per, count(*) nd,
  round(avg(CASE WHEN runner > pick THEN 1.0 ELSE 0 END),3) runner_wins,
  round(avg(runner - pick),2) runner_diff, round(avg(runner-pick)/(stddev(runner-pick)/sqrt(count(*))),2) t_runner,
  count(capd) nd_cap, round(avg(CASE WHEN capd > pick THEN 1.0 ELSE 0 END) FILTER (WHERE capd IS NOT NULL),3) cap_wins,
  round(avg(capd - pick),2) cap_diff,
  round(avg(CASE WHEN nextn > pick THEN 1.0 ELSE 0 END),3) next_wins, round(avg(nextn - pick),2) next_diff,
  round(avg(pick),1) pick_bp
FROM (SELECT *, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per, CAST(year(dd) AS VARCHAR) yr FROM g) z
GROUP BY GROUPING SETS ((per), (yr), ()) ORDER BY per NULLS LAST
