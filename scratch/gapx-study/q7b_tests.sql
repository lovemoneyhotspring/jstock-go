WITH s AS (SELECT *, CASE WHEN dd < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per FROM '/tmp/gapx/sector_daily.parquet'),
l AS (UNPIVOT (SELECT dd, per, sector, s_oc, f_cc1, f_oc1, f_cc5, f_cc20, CAST(f_down AS DOUBLE) f_down, f_rva, f_vol, f_gap FROM s)
      ON f_cc1, f_oc1, f_cc5, f_cc20, f_down, f_rva, f_vol, f_gap INTO NAME feat VALUE val),
rk AS (SELECT *, rank() OVER (PARTITION BY feat, dd ORDER BY val) rv, rank() OVER (PARTITION BY feat, dd ORDER BY s_oc) ry FROM l WHERE val IS NOT NULL),
ic AS (SELECT feat, per, dd, corr(rv, ry) ic FROM rk GROUP BY ALL HAVING count(*) >= 15),
a AS (SELECT feat,
  round(avg(ic) FILTER (WHERE per='IS'), 4) ic_is, round(avg(ic) FILTER (WHERE per='IS') / (stddev(ic) FILTER (WHERE per='IS') / sqrt(count(*) FILTER (WHERE per='IS'))), 2) t_is,
  round(avg(ic) FILTER (WHERE per='OOS'), 4) ic_oos, round(avg(ic) FILTER (WHERE per='OOS') / (stddev(ic) FILTER (WHERE per='OOS') / sqrt(count(*) FILTER (WHERE per='OOS'))), 2) t_oos
  FROM ic GROUP BY feat),
-- (b) 採用に自分の業種の特徴量を当てる
pk AS (
  SELECT p.dd, p.net, p.per, x.sector FROM '/tmp/gapx/picks.parquet' p
  JOIN (SELECT DISTINCT CAST(d AS DATE) dd, code, sector FROM '/tmp/gapx/cand.parquet') x USING (dd, code)
),
pl AS (SELECT pk.per, pk.net, l.feat, l.val FROM pk JOIN l ON l.dd = pk.dd AND l.sector = pk.sector WHERE l.val IS NOT NULL),
th AS (SELECT feat, quantile_cont(val, 0.2) q1, quantile_cont(val, 0.8) q4 FROM pl WHERE per = 'IS' GROUP BY feat),
pb AS (SELECT pl.*, CASE WHEN val <= q1 THEN 'lo' WHEN val > q4 THEN 'hi' END b FROM pl JOIN th USING (feat)),
bt AS (SELECT feat, per,
  avg(net) FILTER (WHERE b='hi') - avg(net) FILTER (WHERE b='lo') d,
  (avg(net) FILTER (WHERE b='hi') - avg(net) FILTER (WHERE b='lo'))
   / sqrt(var_samp(net) FILTER (WHERE b='hi') / count(*) FILTER (WHERE b='hi') + var_samp(net) FILTER (WHERE b='lo') / count(*) FILTER (WHERE b='lo')) t,
  avg(net) FILTER (WHERE b='lo') lo_bp, avg(net) FILTER (WHERE b='hi') hi_bp
  FROM pb GROUP BY ALL),
b AS (SELECT feat,
  round(max(lo_bp) FILTER (WHERE per='IS'),1) lo_is, round(max(hi_bp) FILTER (WHERE per='IS'),1) hi_is, round(max(t) FILTER (WHERE per='IS'),2) bt_is,
  round(max(lo_bp) FILTER (WHERE per='OOS'),1) lo_oos, round(max(hi_bp) FILTER (WHERE per='OOS'),1) hi_oos, round(max(t) FILTER (WHERE per='OOS'),2) bt_oos
  FROM bt GROUP BY feat)
SELECT * FROM a JOIN b USING (feat) ORDER BY abs(t_is) + abs(t_oos) DESC
