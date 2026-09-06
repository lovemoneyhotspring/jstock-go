WITH tp AS (
  SELECT CAST(Date AS DATE) d, TRY_CAST(C AS DOUBLE) c, ROW_NUMBER() OVER (ORDER BY Date) rn,
    AVG(TRY_CAST(C AS DOUBLE)) OVER (ORDER BY Date ROWS BETWEEN 199 PRECEDING AND CURRENT ROW) sma200,
    AVG(TRY_CAST(C AS DOUBLE)) OVER (ORDER BY Date ROWS BETWEEN 19 PRECEDING AND CURRENT ROW) sma20
  FROM indices_bars_daily_topix
),
sr AS (
  SELECT CAST(Date AS DATE) d,
    SUM(TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE)) /
    NULLIF(SUM(TRY_CAST(SellExShortVa AS DOUBLE) + TRY_CAST(ShrtWithResVa AS DOUBLE) + TRY_CAST(ShrtNoResVa AS DOUBLE)), 0) ratio
  FROM markets_short_ratio GROUP BY d
),
feat AS (SELECT d, ratio, (ratio - AVG(ratio) OVER w) / NULLIF(STDDEV(ratio) OVER w, 0) z FROM sr WINDOW w AS (ORDER BY d ROWS BETWEEN 60 PRECEDING AND 1 PRECEDING)),
ret AS (
  SELECT f.d, f.z, t0.c > t0.sma200 up, t0.c / t0.sma20 - 1 dev20, t5.c/t1.c-1 r5, t10.c/t1.c-1 r10, t20.c/t1.c-1 r20
  FROM feat f JOIN tp t0 ON t0.d = f.d JOIN tp t1 ON t1.rn = t0.rn+1 JOIN tp t5 ON t5.rn = t0.rn+1+5
       JOIN tp t10 ON t10.rn = t0.rn+1+10 JOIN tp t20 ON t20.rn = t0.rn+1+20
  WHERE f.z IS NOT NULL AND t0.rn > 200
),
q AS (SELECT *, CASE WHEN z >= 1.5 THEN '3 z≥1.5' WHEN z >= 0.5 THEN '2 z 0.5〜1.5' WHEN z > -0.5 THEN '1 z -0.5〜0.5' ELSE '0 z≤-0.5' END zb,
             CASE WHEN dev20 < -0.03 THEN '20日線 -3%超 下' WHEN dev20 < 0 THEN '20日線 下' ELSE '20日線 上' END pos FROM ret)
SELECT zb, CASE WHEN up THEN 'SMA200 上' ELSE 'SMA200 下' END trend, COUNT(*) n, ROUND(AVG(r5)*100,2) r5, ROUND(AVG(r10)*100,2) r10, ROUND(AVG(r20)*100,2) r20,
  ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) win10
FROM q GROUP BY zb, up
UNION ALL
SELECT zb, pos, COUNT(*), ROUND(AVG(r5)*100,2), ROUND(AVG(r10)*100,2), ROUND(AVG(r20)*100,2), ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1)
FROM q GROUP BY zb, pos
ORDER BY 2, 1
