-- 投資部門別（週次、東証・名証合計）: 個人・外国人の買い越し（売買代金比）の 52 週 z スコアで
-- 公表日翌営業日の TOPIX から 5/10/20 営業日後のリターンを五分位に分ける
WITH tp AS (
  SELECT CAST(Date AS DATE) d, TRY_CAST(C AS DOUBLE) c, ROW_NUMBER() OVER (ORDER BY Date) rn
  FROM indices_bars_daily_topix
),
inv AS (
  SELECT CAST(PubDate AS DATE) pub,
         TRY_CAST(IndBal AS DOUBLE) / NULLIF(TRY_CAST(TotTot AS DOUBLE), 0) ind_pct,
         TRY_CAST(FrgnBal AS DOUBLE) / NULLIF(TRY_CAST(TotTot AS DOUBLE), 0) frgn_pct
  FROM equities_investor_types WHERE Section = 'TokyoNagoya'
),
feat AS (
  SELECT pub, ind_pct, frgn_pct,
    (ind_pct - AVG(ind_pct) OVER w) / NULLIF(STDDEV(ind_pct) OVER w, 0) ind_z,
    (frgn_pct - AVG(frgn_pct) OVER w) / NULLIF(STDDEV(frgn_pct) OVER w, 0) frgn_z
  FROM inv WINDOW w AS (ORDER BY pub ROWS BETWEEN 52 PRECEDING AND 1 PRECEDING)
),
entry AS (SELECT f.*, (SELECT MIN(rn) FROM tp WHERE tp.d > f.pub) rn0 FROM feat f),
ret AS (
  SELECT e.*, t5.c/t0.c-1 r5, t10.c/t0.c-1 r10, t20.c/t0.c-1 r20
  FROM entry e JOIN tp t0 ON t0.rn = e.rn0 JOIN tp t5 ON t5.rn = e.rn0+5
       JOIN tp t10 ON t10.rn = e.rn0+10 JOIN tp t20 ON t20.rn = e.rn0+20
  WHERE e.ind_z IS NOT NULL
),
q AS (SELECT *, NTILE(5) OVER (ORDER BY ind_z) ind_q, NTILE(5) OVER (ORDER BY frgn_z) frgn_q FROM ret)
SELECT '個人' who, ind_q q, COUNT(*) n, ROUND(AVG(ind_z),2) z, ROUND(AVG(r5)*100,2) r5, ROUND(AVG(r10)*100,2) r10, ROUND(AVG(r20)*100,2) r20, ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) win10 FROM q GROUP BY ind_q
UNION ALL
SELECT '外国人', frgn_q, COUNT(*), ROUND(AVG(frgn_z),2), ROUND(AVG(r5)*100,2), ROUND(AVG(r10)*100,2), ROUND(AVG(r20)*100,2), ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) FROM q GROUP BY frgn_q
UNION ALL
SELECT '全体', 0, COUNT(*), 0, ROUND(AVG(r5)*100,2), ROUND(AVG(r10)*100,2), ROUND(AVG(r20)*100,2), ROUND(AVG(CASE WHEN r10>0 THEN 1.0 ELSE 0 END)*100,1) FROM q
ORDER BY who, q
