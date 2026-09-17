-- 採用（上位 3）を前日の騰落で分ける。前日の騰落 = 前日終値 ÷ 前々日終値 − 1（パネルの連続する 2 行で取る）
WITH p AS (
  SELECT CAST(d AS DATE) dd, code, c, prev_close,
         lag(c) OVER (PARTITION BY code ORDER BY d) lag_c,
         lag(prev_close) OVER (PARTITION BY code ORDER BY d) lag_prev
  FROM read_csv('/tmp/minute/panel10.csv', types={'code':'VARCHAR'})
),
r AS (  -- lag_c = prev_close で前日の行が連続していることを確かめる
  SELECT dd, code, CASE WHEN abs(lag_c - prev_close) < 1e-9 AND lag_prev > 0 THEN prev_close / lag_prev - 1 END ret1
  FROM p
),
c AS (SELECT x.*, CAST(x.d AS DATE) dd2 FROM '/tmp/gapx/cand.parquet' x),
s AS (SELECT *, row_number() OVER (PARTITION BY dd2, sector ORDER BY key, code) sr FROM c),
t AS (SELECT *, row_number() OVER (PARTITION BY dd2 ORDER BY key, code) pr FROM s WHERE sr = 1),
pk AS (SELECT t.*, r.ret1 FROM t LEFT JOIN r ON r.dd = t.dd2 AND r.code = t.code WHERE pr <= 3),
b AS (
  SELECT *, CASE WHEN ret1 IS NULL THEN '0:不明'
                 WHEN ret1 <= -0.03 THEN '1:前日 -3%以下（大きく続落）'
                 WHEN ret1 < 0 THEN '2:前日 -3〜0%（続落）'
                 WHEN ret1 < 0.03 THEN '3:前日 0〜+3%'
                 ELSE '4:前日 +3%以上（上げてから下げ）' END bin,
         CASE WHEN dd2 < DATE '2022-01-01' THEN 'IS' ELSE 'OOS' END per
  FROM pk
)
SELECT bin, count(*) n, round(avg(net),1) bp, round(avg(net)/(stddev(net)/sqrt(count(*))),2) t,
  round(avg(CASE WHEN net > 0 THEN 1.0 ELSE 0 END),3) win,
  round(avg(net) FILTER (WHERE per='IS'),1) is_bp, count(*) FILTER (WHERE per='IS') n_is,
  round(avg(net) FILTER (WHERE per='OOS'),1) oos_bp, count(*) FILTER (WHERE per='OOS') n_oos
FROM b GROUP BY ROLLUP(bin) ORDER BY bin NULLS LAST
