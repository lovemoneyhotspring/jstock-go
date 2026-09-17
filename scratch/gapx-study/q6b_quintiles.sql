WITH f AS (SELECT * FROM '/tmp/gapx/picks.parquet'),
long AS (
  UNPIVOT (SELECT dd, per, lose, net, gap, vol20, key, ln(prev_close) lnpx, sector_gap, r1, r5, r20, rng, cpos, rva, sma25, lnmcap, spx, vix, tpx_oc, sec_oc, up_max, dn_max FROM f)
  ON gap, vol20, key, lnpx, sector_gap, r1, r5, r20, rng, cpos, rva, sma25, lnmcap, spx, vix, tpx_oc, sec_oc, up_max, dn_max INTO NAME feat VALUE val
),
th AS (SELECT feat, quantile_cont(val, [0.2, 0.4, 0.6, 0.8]) q FROM long WHERE per = 'IS' GROUP BY feat),
b AS (SELECT l.*, CASE WHEN val <= q[1] THEN 1 WHEN val <= q[2] THEN 2 WHEN val <= q[3] THEN 3 WHEN val <= q[4] THEN 4 ELSE 5 END qn, q
      FROM long l JOIN th USING (feat) WHERE val IS NOT NULL),
agg AS (
  SELECT feat, per, qn, avg(lose) lr FROM b GROUP BY ALL
),
tt AS (
  SELECT feat, per,
    (avg(val) FILTER (WHERE lose = 1) - avg(val) FILTER (WHERE lose = 0))
      / sqrt(var_samp(val) FILTER (WHERE lose = 1) / count(*) FILTER (WHERE lose = 1)
           + var_samp(val) FILTER (WHERE lose = 0) / count(*) FILTER (WHERE lose = 0)) t
  FROM b GROUP BY ALL
)
SELECT feat,
  round(any_value(q)[1], 4) q20, round(any_value(q)[4], 4) q80,
  string_agg(round(lr*100)::INT::VARCHAR, '/' ORDER BY qn) FILTER (WHERE per='IS') is_q1_q5,
  string_agg(round(lr*100)::INT::VARCHAR, '/' ORDER BY qn) FILTER (WHERE per='OOS') oos_q1_q5,
  round(max(t) FILTER (WHERE per='IS'), 1) t_is, round(max(t) FILTER (WHERE per='OOS'), 1) t_oos
FROM agg JOIN tt USING (feat, per) JOIN th USING (feat)
GROUP BY feat ORDER BY abs(max(t) FILTER (WHERE per='IS')) + abs(max(t) FILTER (WHERE per='OOS')) DESC
