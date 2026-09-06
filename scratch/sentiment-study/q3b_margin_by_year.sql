, m0 AS (
  SELECT Code, CAST(Date AS DATE) wd, TRY_CAST(LongVol AS DOUBLE) lv, TRY_CAST(ShrtVol AS DOUBLE) sv
  FROM markets_margin_interest WHERE Code IN (SELECT sym || '0' FROM uni)
),
j AS (
  SELECT m.*, f.grp, f.d, f.v20, f.x10, f.x20, ROW_NUMBER() OVER (PARTITION BY m.Code, m.wd ORDER BY f.d) k
  FROM m0 m JOIN fwd f ON f.Code = m.Code AND f.d BETWEEN m.wd + INTERVAL 5 DAY AND m.wd + INTERVAL 9 DAY
),
feat AS (SELECT grp, Code, wd, x10, x20, lv / NULLIF(sv, 0) ratio, sv / NULLIF(v20, 0) shortdays FROM j WHERE k = 1 AND v20 > 0),
q AS (
  SELECT *, NTILE(5) OVER (PARTITION BY grp, wd ORDER BY ratio) q_ratio, NTILE(5) OVER (PARTITION BY grp, wd ORDER BY shortdays) q_sd
  FROM feat WHERE ratio IS NOT NULL AND shortdays IS NOT NULL
)
SELECT YEAR(wd) y, grp, COUNT(*)/5 n_per_q,
  ROUND(AVG(CASE WHEN q_sd = 1 THEN x20 END)*100, 2) sd_q1, ROUND(AVG(CASE WHEN q_sd = 5 THEN x20 END)*100, 2) sd_q5,
  ROUND((AVG(CASE WHEN q_sd = 1 THEN x20 END) - AVG(CASE WHEN q_sd = 5 THEN x20 END))*100, 2) sd_spread,
  ROUND(AVG(CASE WHEN q_ratio = 1 THEN x20 END)*100, 2) ratio_q1, ROUND(AVG(CASE WHEN q_ratio = 5 THEN x20 END)*100, 2) ratio_q5,
  ROUND((AVG(CASE WHEN q_ratio = 5 THEN x20 END) - AVG(CASE WHEN q_ratio = 1 THEN x20 END))*100, 2) ratio_spread
FROM q GROUP BY y, grp ORDER BY grp, y
