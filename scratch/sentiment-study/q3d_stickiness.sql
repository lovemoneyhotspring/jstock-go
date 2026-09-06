, m0 AS (
  SELECT Code, CAST(Date AS DATE) wd, TRY_CAST(LongVol AS DOUBLE) / NULLIF(TRY_CAST(ShrtVol AS DOUBLE), 0) ratio
  FROM markets_margin_interest WHERE Code IN (SELECT sym || '0' FROM uni)
),
q AS (SELECT Code, wd, NTILE(5) OVER (PARTITION BY wd ORDER BY ratio) q FROM m0 WHERE ratio IS NOT NULL),
lagged AS (
  SELECT a.Code, a.wd, a.q q0, b4.q q4, b13.q q13, b52.q q52
  FROM q a LEFT JOIN q b4 ON b4.Code = a.Code AND b4.wd = a.wd + INTERVAL 28 DAY
           LEFT JOIN q b13 ON b13.Code = a.Code AND b13.wd = a.wd + INTERVAL 91 DAY
           LEFT JOIN q b52 ON b52.Code = a.Code AND b52.wd = a.wd + INTERVAL 364 DAY
)
SELECT q0, COUNT(*) n,
  ROUND(AVG(CASE WHEN q4 = q0 THEN 1.0 ELSE 0 END)*100,1) same_4w, ROUND(AVG(CASE WHEN q13 = q0 THEN 1.0 ELSE 0 END)*100,1) same_13w,
  ROUND(AVG(CASE WHEN q52 = q0 THEN 1.0 ELSE 0 END)*100,1) same_52w, ROUND(CORR(q0, q52), 2) corr_52w
FROM lagged GROUP BY q0 ORDER BY q0
