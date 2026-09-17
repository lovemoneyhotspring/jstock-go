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
    count(*) FILTER (WHERE sr >= 2 AND ov < ov3) n_cap,
    avg(key) FILTER (WHERE (sr >= 2 AND ov < ov3) OR pr BETWEEN 4 AND 6) - avg(key) FILTER (WHERE pr <= 3) closeness,
    avg(vol20) FILTER (WHERE (sr >= 2 AND ov < ov3) OR pr BETWEEN 4 AND 6) - avg(vol20) FILTER (WHERE pr <= 3) vol_diff
  FROM x JOIN cut USING (dd) WHERE cut.ov3 IS NOT NULL GROUP BY x.dd
),
mk AS (
  SELECT CAST(d AS DATE) dd, median(gap) med_gap, avg(CASE WHEN gap < -0.02 THEN 1.0 ELSE 0 END) breadth_down
  FROM read_csv('/tmp/minute/panel10.csv', types={'code':'VARCHAR'})
  WHERE eligible AND o > 0 AND prev_close > 0 GROUP BY 1
),
us AS (
  SELECT CAST(date AS DATE) ud, spx/lag(spx) OVER (ORDER BY date) - 1 spx, vix
  FROM read_json('data/daytrade/us.json')
),
f0 AS (
  SELECT g.*, runner - pick diff, mk.med_gap, mk.breadth_down, us.spx, us.vix
  FROM g JOIN mk USING (dd) ASOF LEFT JOIN us ON g.dd > us.ud
  WHERE runner IS NOT NULL
),
f AS (
  SELECT *, avg(diff) OVER (ORDER BY dd ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) trail20,
         CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per
  FROM f0
),
long AS (
  UNPIVOT (SELECT dd, per, diff, CAST(n_cap AS DOUBLE) n_cap, closeness, vol_diff, med_gap, breadth_down, spx, vix, trail20 FROM f)
  ON n_cap, closeness, vol_diff, med_gap, breadth_down, spx, vix, trail20 INTO NAME feat VALUE val
),
th AS (
  SELECT feat, quantile_cont(val, 1/3) q1, quantile_cont(val, 2/3) q2 FROM long WHERE per = 'IS' GROUP BY feat
),
b AS (
  SELECT l.*, CASE
    WHEN l.feat = 'n_cap' THEN CASE WHEN val = 0 THEN '1:0' WHEN val = 1 THEN '2:1' ELSE '3:2+' END
    WHEN val <= q1 THEN '1:low' WHEN val <= q2 THEN '2:mid' ELSE '3:high' END bin,
    th.q1, th.q2
  FROM long l JOIN th USING (feat)
)
SELECT feat, bin, round(any_value(q1),4) q1, round(any_value(q2),4) q2,
  count(*) FILTER (WHERE per='IS') n_is,
  round(avg(diff) FILTER (WHERE per='IS'),1) is_diff,
  round(avg(diff) FILTER (WHERE per='IS')/(stddev(diff) FILTER (WHERE per='IS')/sqrt(count(*) FILTER (WHERE per='IS'))),2) is_t,
  round(avg(CASE WHEN diff>0 THEN 1.0 ELSE 0 END) FILTER (WHERE per='IS'),3) is_win,
  count(*) FILTER (WHERE per='OOS') n_oos,
  round(avg(diff) FILTER (WHERE per='OOS'),1) oos_diff,
  round(avg(diff) FILTER (WHERE per='OOS')/(stddev(diff) FILTER (WHERE per='OOS')/sqrt(count(*) FILTER (WHERE per='OOS'))),2) oos_t,
  round(avg(CASE WHEN diff>0 THEN 1.0 ELSE 0 END) FILTER (WHERE per='OOS'),3) oos_win
FROM b WHERE val IS NOT NULL GROUP BY feat, bin ORDER BY feat, bin
