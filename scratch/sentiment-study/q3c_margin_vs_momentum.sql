, mom AS (
  SELECT Code, d, c / LAG(c, 20) OVER (PARTITION BY Code ORDER BY d) - 1 ret20, c / LAG(c, 120) OVER (PARTITION BY Code ORDER BY d) - 1 ret120 FROM bars
),
m0 AS (
  SELECT Code, CAST(Date AS DATE) wd, TRY_CAST(LongVol AS DOUBLE) lv, TRY_CAST(ShrtVol AS DOUBLE) sv
  FROM markets_margin_interest WHERE Code IN (SELECT sym || '0' FROM uni)
),
j AS (
  SELECT m.*, f.grp, f.d, f.v20, f.x10, f.x20, f.r20, mo.ret20, mo.ret120, ROW_NUMBER() OVER (PARTITION BY m.Code, m.wd ORDER BY f.d) k
  FROM m0 m JOIN fwd f ON f.Code = m.Code AND f.d BETWEEN m.wd + INTERVAL 5 DAY AND m.wd + INTERVAL 9 DAY
       JOIN mom mo ON mo.Code = m.Code AND mo.d = m.wd
),
feat AS (SELECT grp, Code, wd, x10, x20, ret20, ret120, lv / NULLIF(sv, 0) ratio FROM j WHERE k = 1 AND v20 > 0 AND ret20 IS NOT NULL),
q AS (
  SELECT *, NTILE(5) OVER (PARTITION BY grp, wd ORDER BY ratio) q_ratio, NTILE(3) OVER (PARTITION BY grp, wd ORDER BY ret20) q_mom,
    NTILE(3) OVER (PARTITION BY grp, wd ORDER BY ret120) q_mom120
  FROM feat WHERE ratio IS NOT NULL
)
SELECT '過去20日' mom_kind, grp, q_mom mom_q, COUNT(*)/5 n_per_q, ROUND(AVG(ret20)*100,1) mom_ret,
  ROUND(AVG(CASE WHEN q_ratio = 1 THEN x20 END)*100, 2) ratio_q1, ROUND(AVG(CASE WHEN q_ratio = 3 THEN x20 END)*100, 2) ratio_q3,
  ROUND(AVG(CASE WHEN q_ratio = 5 THEN x20 END)*100, 2) ratio_q5,
  ROUND((AVG(CASE WHEN q_ratio = 5 THEN x20 END) - AVG(CASE WHEN q_ratio = 1 THEN x20 END))*100, 2) spread
FROM q GROUP BY grp, q_mom
UNION ALL
SELECT '過去120日', grp, q_mom120, COUNT(*)/5, ROUND(AVG(ret120)*100,1),
  ROUND(AVG(CASE WHEN q_ratio = 1 THEN x20 END)*100, 2), ROUND(AVG(CASE WHEN q_ratio = 3 THEN x20 END)*100, 2),
  ROUND(AVG(CASE WHEN q_ratio = 5 THEN x20 END)*100, 2),
  ROUND((AVG(CASE WHEN q_ratio = 5 THEN x20 END) - AVG(CASE WHEN q_ratio = 1 THEN x20 END))*100, 2)
FROM q GROUP BY grp, q_mom120
ORDER BY mom_kind, grp, mom_q
