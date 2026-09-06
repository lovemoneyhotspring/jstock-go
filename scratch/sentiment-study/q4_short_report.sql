, rep AS (
  SELECT Code, CAST(DiscDate AS DATE) dd,
    SUM(TRY_CAST(ShrtPosToSO AS DOUBLE) - COALESCE(TRY_CAST(PrevRptRatio AS DOUBLE), 0)) flow,
    SUM(TRY_CAST(ShrtPosToSO AS DOUBLE)) lvl, COUNT(*) reports
  FROM markets_short_sale_report WHERE Code IN (SELECT Code FROM uni)
  GROUP BY Code, dd
),
j AS (
  SELECT r.*, f.grp, f.d, f.x5, f.x10, f.x20, ROW_NUMBER() OVER (PARTITION BY r.Code, r.dd ORDER BY f.d) k
  FROM rep r JOIN fwd f ON f.Code = r.Code AND f.d > r.dd AND f.d <= r.dd + INTERVAL 7 DAY
),
feat AS (
  SELECT *, CASE WHEN flow <= -0.005 THEN '1 解消 ≤-0.5%' WHEN flow < 0 THEN '2 解消 -0.5〜0' WHEN flow = 0 THEN '3 変化なし'
                 WHEN flow < 0.005 THEN '4 積増 0〜0.5%' ELSE '5 積増 ≥0.5%' END bucket
  FROM j WHERE k = 1
)
SELECT bucket, grp, COUNT(*) n, ROUND(AVG(flow)*100,2) flow_pct, ROUND(AVG(x5)*100,2) x5, ROUND(AVG(x10)*100,2) x10, ROUND(AVG(x20)*100,2) x20,
       ROUND(AVG(CASE WHEN x10>0 THEN 1.0 ELSE 0 END)*100,1) win10
FROM feat GROUP BY bucket, grp ORDER BY bucket, grp
