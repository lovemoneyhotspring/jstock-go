"""寄り前の気配が、始値のギャップの日内の順位をどれだけ再現するかを時刻ごとに測る。

根拠: vault 20-research/2026-09-jp-daytrade-preopen-order.md「寄り前の時刻ごとの順位の再現度」
（2026-09-20 にその場の SQL で測ったものをスクリプトにした。8:59:45 の記録が 20 営業日
溜まったら、これで測り直す）

  test/.venv/bin/python test/dt_preopen_rank.py [--since 2026-09-11] [--daily]

母集団: 当日の売買代金 1 億以上・始値のギャップが負の銘柄。
見える値段: 買い気配と売り気配が両方あれば中値、片方だけならその値（preopen-order の検証と同じ）。
並べ替えは日内の順位しか使わないので、指標は誤差の大きさではなく順位の再現度:
  spearman  見えるギャップと始値のギャップの日内の順位相関（日ごとに出して平均）
  top20     始値のギャップが最も深い 20 銘柄のうち、見えるギャップでも上位 20 に入る数
  e_all / e_deep  絶対誤差の中央値（%pt）。deep は始値のギャップが −3% 以下
  bias_deep       誤差（見える − 始値）の中央値。正なら浅く見えている
"""

import argparse

import duckdb

BOOK = "state/daytrade/history/book/*.parquet"
BARS = "data/jquants/equities_bars_daily/*.parquet"
SLOTS = ("0830", "0845", "0855", "0859", "085945")
MIN_TURNOVER = 1e8
DEEP = -3.0
TOP = 20

SQL = f"""
WITH b AS (
  SELECT CAST(day AS DATE) d, slot, symbol, TRY_CAST(pPRP AS DOUBLE) pc,
         TRY_CAST(pQAP AS DOUBLE) ask, TRY_CAST(pQBP AS DOUBLE) bid
  FROM read_parquet('{BOOK}', union_by_name=true)
  WHERE slot IN {SLOTS} AND CAST(day AS DATE) >= CAST(? AS DATE)),
v AS (
  SELECT d, slot, symbol, pc,
         CASE WHEN ask > 0 AND bid > 0 THEN (ask + bid) / 2 WHEN ask > 0 THEN ask WHEN bid > 0 THEN bid END vis
  FROM b WHERE pc > 0),
q AS (
  SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(O AS DOUBLE) op, TRY_CAST(Va AS DOUBLE) va
  FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= CAST(? AS DATE)),
n AS (
  SELECT v.d, v.slot, (v.vis / v.pc - 1) * 100 gv, (q.op / v.pc - 1) * 100 g
  FROM v JOIN q ON q.d = v.d AND q.code = v.symbol || '0'
  WHERE v.vis IS NOT NULL AND q.op > 0 AND q.va >= {MIN_TURNOVER} AND q.op < v.pc),
r AS (
  SELECT *, rank() OVER (PARTITION BY d, slot ORDER BY gv) rv, rank() OVER (PARTITION BY d, slot ORDER BY g) rg FROM n)
SELECT d, slot, count(*) n_names, corr(rv, rg) spearman, sum((rv <= {TOP} AND rg <= {TOP})::INT) top20,
       median(abs(gv - g)) e_all, median(abs(gv - g)) FILTER (WHERE g <= {DEEP}) e_deep,
       median(gv - g) FILTER (WHERE g <= {DEEP}) bias_deep, count(*) FILTER (WHERE g <= {DEEP}) n_deep
FROM r GROUP BY d, slot ORDER BY d, slot
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-11")
    ap.add_argument("--daily", action="store_true", help="日ごとの行も出す")
    a = ap.parse_args()
    df = duckdb.sql(SQL, params=[a.since, a.since]).df()
    if df.empty:
        print("板の記録と日足が揃った日がありません")
        return
    if a.daily:
        print(df.round(3).to_string(index=False))
        print()
    # 誤差の中央値は日ごとの中央値の平均（日で重みを揃える）
    agg = df.groupby("slot").agg(days=("d", "nunique"), names=("n_names", "mean"), spearman=("spearman", "mean"),
                                 top20=("top20", "mean"), e_all=("e_all", "mean"), e_deep=("e_deep", "mean"),
                                 bias_deep=("bias_deep", "mean"), n_deep=("n_deep", "sum"))
    print(f"{df['d'].min():%Y-%m-%d}〜{df['d'].max():%Y-%m-%d}")
    print(agg.round(3).to_string())


if __name__ == "__main__":
    main()
