"""daytrade の候補表の空売り残高を「判定日の前日までに公表されたもの」だけで作る（plan の loadShortInterest と同じ意味）。

各 (code, 公表日 D) で、D までに公表された行の最新の計算日 lc と、その計算日の報告の合計を持ち、
判定日 d より前の最新の D を ASOF で当てる。lc の行の公表が d の 120 日より前なら無し（plan の窓）。
backtest のパネル（panelCTEs の ssr）と同じ定義。

  test/.venv/bin/python test/dt_si_pit.py <候補表> <出力>   # リポジトリの直下で実行
"""
import sys
import duckdb
import pandas as pd

cand_in, cand_out = sys.argv[1], sys.argv[2]
con = duckdb.connect()
con.execute("SET memory_limit='6GB'")
con.execute("""
CREATE TABLE ssr AS
SELECT CAST(Code AS VARCHAR) code, CAST(CalcDate AS DATE) cd, CAST(DiscDate AS DATE) dd,
       TRY_CAST(ShrtPosToSO AS DOUBLE) v
FROM read_parquet('data/jquants/markets_short_sale_report/**/*.parquet', union_by_name=true)
WHERE Code IS NOT NULL AND CalcDate IS NOT NULL AND DiscDate IS NOT NULL
""")
con.execute("""
CREATE TABLE states AS
WITH d AS (
  SELECT code, dd, max(cd) AS mc FROM ssr GROUP BY 1, 2
),
s AS (
  SELECT code, dd, max(mc) OVER (PARTITION BY code ORDER BY dd ROWS UNBOUNDED PRECEDING) AS lc FROM d
)
SELECT s.code, s.dd, s.lc, sum(r.v) AS si, max(r.dd) AS ld
FROM s JOIN ssr r ON r.code = s.code AND r.cd = s.lc AND r.dd <= s.dd
GROUP BY 1, 2, 3
""")
cand = pd.read_parquet(cand_in)
cand["d"] = pd.to_datetime(cand["d"])
con.register("cand_df", cand[["d", "code"]])
pit = con.execute("""
SELECT c.d, c.code,
       CASE WHEN st.ld >= CAST(c.d AS DATE) - INTERVAL 120 DAY THEN st.si END AS si_pit
FROM cand_df c
ASOF LEFT JOIN states st ON st.code = c.code AND st.dd < CAST(c.d AS DATE)
""").df()
pit["d"] = pd.to_datetime(pit["d"])
out = cand.merge(pit, on=["d", "code"], how="left")
print("差し替え前後で値がある行", out["short_interest"].notna().mean(), out["si_pit"].notna().mean())
out["short_interest_lookahead"] = out["short_interest"]
out["short_interest"] = out["si_pit"]
out = out.drop(columns=["si_pit"])
out.to_parquet(cand_out)
print("書いた", cand_out, len(out))
