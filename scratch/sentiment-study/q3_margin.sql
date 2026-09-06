, m0 AS (
  SELECT Code, CAST(Date AS DATE) wd, TRY_CAST(LongVol AS DOUBLE) lv, TRY_CAST(ShrtVol AS DOUBLE) sv
  FROM markets_margin_interest WHERE Code IN (SELECT Code FROM uni)
),
m1 AS (
  SELECT *, lv - LAG(lv) OVER (PARTITION BY Code ORDER BY wd) dlv, sv - LAG(sv) OVER (PARTITION BY Code ORDER BY wd) dsv
  FROM m0
),
-- 金曜の残高は翌週火曜に公表 → 水曜（wd+5 日）以降の最初の営業日の寄付で建てる
j AS (
  SELECT m.*, f.grp, f.d, f.v20, f.c, f.x5, f.x10, f.x20,
    ROW_NUMBER() OVER (PARTITION BY m.Code, m.wd ORDER BY f.d) k
  FROM m1 m JOIN fwd f ON f.Code = m.Code AND f.d BETWEEN m.wd + INTERVAL 5 DAY AND m.wd + INTERVAL 9 DAY
),
feat AS (
  SELECT grp, Code, wd, x5, x10, x20,
    lv / NULLIF(sv, 0) ratio,
    dlv / NULLIF(v20, 0) dlong,
    dsv / NULLIF(v20, 0) dshort,
    lv / NULLIF(v20, 0) longdays,
    sv / NULLIF(v20, 0) shortdays
  FROM j WHERE k = 1 AND v20 > 0
),
q AS (
  SELECT *,
    NTILE(5) OVER (PARTITION BY grp, wd ORDER BY ratio) q_ratio,
    NTILE(5) OVER (PARTITION BY grp, wd ORDER BY dlong) q_dlong,
    NTILE(5) OVER (PARTITION BY grp, wd ORDER BY dshort) q_dshort,
    NTILE(5) OVER (PARTITION BY grp, wd ORDER BY longdays) q_longdays,
    NTILE(5) OVER (PARTITION BY grp, wd ORDER BY shortdays) q_shortdays
  FROM feat WHERE ratio IS NOT NULL AND dlong IS NOT NULL
)
SELECT feature, grp, q, COUNT(*) n, ROUND(AVG(x5)*100,2) x5, ROUND(AVG(x10)*100,2) x10, ROUND(AVG(x20)*100,2) x20,
       ROUND(AVG(CASE WHEN x10>0 THEN 1.0 ELSE 0 END)*100,1) win10
FROM (
  SELECT '1 信用倍率(買残/売残)' feature, grp, q_ratio q, x5, x10, x20 FROM q
  UNION ALL SELECT '2 買残の増減/出来高', grp, q_dlong, x5, x10, x20 FROM q
  UNION ALL SELECT '3 売残の増減/出来高', grp, q_dshort, x5, x10, x20 FROM q
  UNION ALL SELECT '4 買残/出来高(日数)', grp, q_longdays, x5, x10, x20 FROM q
  UNION ALL SELECT '5 売残/出来高(日数)', grp, q_shortdays, x5, x10, x20 FROM q
) GROUP BY feature, grp, q ORDER BY feature, grp, q
