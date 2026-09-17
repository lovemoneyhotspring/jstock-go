COPY (
WITH p AS (
  SELECT * FROM read_csv('/tmp/minute/panel10.csv', types={'code':'VARCHAR'})
  WHERE eligible AND o > 0 AND c > 0 AND prev_close > 0
),
m AS (
  SELECT CAST("Date" AS DATE) AS d, CAST("Code" AS VARCHAR) AS code, CAST("S33" AS VARCHAR) AS sector
  FROM read_parquet('data/jquants/equities_master/*.parquet', union_by_name=true)
  WHERE "Date" >= DATE '2017-01-01'
),
pm AS (
  SELECT p.*, m.sector FROM p LEFT JOIN m ON m.d = CAST(p.d AS DATE) AND m.code = p.code
),
sg AS (
  SELECT d, sector, CASE WHEN count(*) >= 5 THEN median(gap) END AS sector_gap, count(*) AS sector_n
  FROM pm WHERE sector IS NOT NULL GROUP BY d, sector
)
SELECT pm.d, pm.code, pm.sector, pm.o, pm.c, pm.prev_close, pm.vol20, pm.gap,
       sg.sector_gap, sg.sector_n,
       (pm.c/pm.o - 1)*10000 - 5.7 AS net,
       round(pm.gap,4)/greatest(coalesce(pm.vol20,0.02),0.02) AS key,
       CASE WHEN sg.sector_gap IS NOT NULL
            THEN round(pm.gap - sg.sector_gap,4)/greatest(coalesce(pm.vol20,0.02),0.02)
            ELSE round(pm.gap,4)/greatest(coalesce(pm.vol20,0.02),0.02) END AS rkey,
       pm.gap - sg.sector_gap AS resid
FROM pm LEFT JOIN sg ON sg.d = pm.d AND sg.sector = pm.sector
WHERE pm.gap < 0 AND pm.o > pm.limit_low
) TO '/tmp/gapx/cand.parquet' (FORMAT parquet);
