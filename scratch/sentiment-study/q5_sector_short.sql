, tp AS (SELECT CAST(Date AS DATE) d FROM indices_bars_daily_topix),
sec AS (
  SELECT Code, S33 FROM (SELECT Code, S33, ROW_NUMBER() OVER (PARTITION BY Code ORDER BY Date DESC) k FROM equities_master WHERE Code IN (SELECT sym || '0' FROM uni)) WHERE k = 1
),
sr AS (
  SELECT CAST(Date AS DATE) d, S33,
    (TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE)) /
    NULLIF(TRY_CAST(SellExShortVa AS DOUBLE) + TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE), 0) ratio
  FROM markets_short_ratio
),
sz AS (
  SELECT d, S33, ratio, (ratio - AVG(ratio) OVER w) / NULLIF(STDDEV(ratio) OVER w, 0) z
  FROM sr WINDOW w AS (PARTITION BY S33 ORDER BY d ROWS BETWEEN 60 PRECEDING AND 1 PRECEDING)
),
sig AS (SELECT s.*, (SELECT MIN(t.d) FROM tp t WHERE t.d > s.d) entry, NTILE(5) OVER (PARTITION BY d ORDER BY z) q FROM sz s WHERE z IS NOT NULL),
j AS (
  SELECT g.q, g.z, f.grp, f.x5, f.x10, f.x20
  FROM fwd f JOIN sec ON sec.Code = f.Code JOIN sig g ON g.S33 = sec.S33 AND g.entry = f.d
)
SELECT '業種の空売り比率 z 五分位' feature, grp, q, COUNT(*) n, ROUND(AVG(z),2) z, ROUND(AVG(x5)*100,2) x5, ROUND(AVG(x10)*100,2) x10, ROUND(AVG(x20)*100,2) x20,
  ROUND(AVG(CASE WHEN x10>0 THEN 1.0 ELSE 0 END)*100,1) win10
FROM j GROUP BY grp, q ORDER BY grp, q
