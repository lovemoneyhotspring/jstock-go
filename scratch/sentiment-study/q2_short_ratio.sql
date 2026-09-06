-- 空売り比率（日次、全業種合計）: 60 日 z スコアの五分位で翌営業日以降の TOPIX リターン
WITH tp AS (
  SELECT CAST(Date AS DATE) d, TRY_CAST(C AS DOUBLE) c, ROW_NUMBER() OVER (ORDER BY Date) rn
  FROM indices_bars_daily_topix
),
sr AS (
  SELECT CAST(Date AS DATE) d,
    SUM(TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE)) /
    NULLIF(SUM(TRY_CAST(SellExShortVa AS DOUBLE) + TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE)), 0) ratio
  FROM markets_short_ratio GROUP BY d
),
feat AS (
  SELECT d, ratio, (ratio - AVG(ratio) OVER w) / NULLIF(STDDEV(ratio) OVER w, 0) z
  FROM sr WINDOW w AS (ORDER BY d ROWS BETWEEN 60 PRECEDING AND 1 PRECEDING)
),
ret AS (
  SELECT f.d, f.ratio, f.z, t5.c/t1.c-1 r5, t10.c/t1.c-1 r10, t20.c/t1.c-1 r20
  FROM feat f JOIN tp t0 ON t0.d = f.d JOIN tp t1 ON t1.rn = t0.rn+1 JOIN tp t5 ON t5.rn = t0.rn+1+5
       JOIN tp t10 ON t10.rn = t0.rn+1+10 JOIN tp t20 ON t20.rn = t0.rn+1+20
  WHERE f.z IS NOT NULL
),
q AS (SELECT *, NTILE(5) OVER (ORDER BY z) q FROM ret)
SELECT q, COUNT(*) n, ROUND(AVG(ratio)*100,1) ratio_pct, ROUND(AVG(z),2) z, ROUND(AVG(r5)*100,2) r5, ROUND(AVG(r10)*100,2) r10, ROUND(AVG(r20)*100,2) r20,
       ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) win10
FROM q GROUP BY q
UNION ALL
SELECT 0, COUNT(*), ROUND(AVG(ratio)*100,1), 0, ROUND(AVG(r5)*100,2), ROUND(AVG(r10)*100,2), ROUND(AVG(r20)*100,2), ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) FROM q
ORDER BY q
