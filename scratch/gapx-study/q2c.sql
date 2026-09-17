WITH c AS (SELECT *, CAST(d AS DATE) dd FROM '/tmp/gapx/cand.parquet'),
s AS (SELECT *, row_number() OVER (PARTITION BY dd, sector ORDER BY key, code) sr FROM c),
t AS (SELECT *, row_number() OVER (PARTITION BY dd ORDER BY key, code) pr FROM s WHERE sr = 1),
pairs AS (
  SELECT a.dd, a.sector, a.net n1, b.net n2, a.gap g1, b.gap g2, a.vol20 v1, b.vol20 v2, a.key k1, b.key k2,
         a.resid r1, b.resid r2, a.prev_close p1, b.prev_close p2
  FROM t a JOIN s b ON b.dd = a.dd AND b.sector = a.sector AND b.sr = 2
  WHERE a.pr <= 3
)
, sp AS (
  SELECT dd, CASE WHEN n1 > n2 THEN 1 ELSE 0 END first_up,
    CASE WHEN n1 > n2 THEN g1-g2 ELSE g2-g1 END d_gap,
    CASE WHEN n1 > n2 THEN v1-v2 ELSE v2-v1 END d_vol,
    CASE WHEN n1 > n2 THEN k1-k2 ELSE k2-k1 END d_key,
    CASE WHEN n1 > n2 THEN r1-r2 ELSE r2-r1 END d_resid,
    CASE WHEN n1 > n2 THEN ln(p1/p2) ELSE ln(p2/p1) END d_lnpx
  FROM pairs WHERE sign(n1) <> sign(n2) AND r1 IS NOT NULL AND r2 IS NOT NULL AND v1 IS NOT NULL AND v2 IS NOT NULL
)
SELECT count(*) n, round(avg(first_up),3) first_up_share, round(avg(d_gap) FILTER (WHERE dd < DATE '2022-01-01')/(stddev(d_gap) FILTER (WHERE dd < DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd < DATE '2022-01-01'))),2) t_d_gap_is, round(avg(d_gap) FILTER (WHERE dd >= DATE '2022-01-01')/(stddev(d_gap) FILTER (WHERE dd >= DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd >= DATE '2022-01-01'))),2) t_d_gap_oos, round(avg(d_gap),4) m_d_gap, round(avg(d_vol) FILTER (WHERE dd < DATE '2022-01-01')/(stddev(d_vol) FILTER (WHERE dd < DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd < DATE '2022-01-01'))),2) t_d_vol_is, round(avg(d_vol) FILTER (WHERE dd >= DATE '2022-01-01')/(stddev(d_vol) FILTER (WHERE dd >= DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd >= DATE '2022-01-01'))),2) t_d_vol_oos, round(avg(d_vol),4) m_d_vol, round(avg(d_key) FILTER (WHERE dd < DATE '2022-01-01')/(stddev(d_key) FILTER (WHERE dd < DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd < DATE '2022-01-01'))),2) t_d_key_is, round(avg(d_key) FILTER (WHERE dd >= DATE '2022-01-01')/(stddev(d_key) FILTER (WHERE dd >= DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd >= DATE '2022-01-01'))),2) t_d_key_oos, round(avg(d_key),4) m_d_key, round(avg(d_resid) FILTER (WHERE dd < DATE '2022-01-01')/(stddev(d_resid) FILTER (WHERE dd < DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd < DATE '2022-01-01'))),2) t_d_resid_is, round(avg(d_resid) FILTER (WHERE dd >= DATE '2022-01-01')/(stddev(d_resid) FILTER (WHERE dd >= DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd >= DATE '2022-01-01'))),2) t_d_resid_oos, round(avg(d_resid),4) m_d_resid, round(avg(d_lnpx) FILTER (WHERE dd < DATE '2022-01-01')/(stddev(d_lnpx) FILTER (WHERE dd < DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd < DATE '2022-01-01'))),2) t_d_lnpx_is, round(avg(d_lnpx) FILTER (WHERE dd >= DATE '2022-01-01')/(stddev(d_lnpx) FILTER (WHERE dd >= DATE '2022-01-01')/sqrt(count(*) FILTER (WHERE dd >= DATE '2022-01-01'))),2) t_d_lnpx_oos, round(avg(d_lnpx),4) m_d_lnpx FROM sp