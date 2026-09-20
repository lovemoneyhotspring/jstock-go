"""9:00 の open の回を早めたとき、時価問合に始値がもう入っていたかを朝ごとに確かめる。

DT_OPEN_AT（deploy/crontab.txt）を刻んで早めている途中の点検用（2026-09-20〜）。始値が入っていない銘柄は
「まだ寄っていない」扱いになり、値段を板の気配から作る（from_book）。寄る前の気配は誤差が大きいので、
早めすぎると選定がぶれる。物差しは「その朝の 1 回目で寄っていた数 ÷ 次の回（9:00:25 ごろ）で寄っていた数」。
基準: 2026-09-18（開始 9:00:04.1・気配の取り終わり 9:00:05.9）で 869 / 878 = 99.0%。

  test/.venv/bin/python test/dt_open_quote_timing.py [--since 2026-09-16]

出力の missed は「次の回では寄っていたのに 1 回目では寄っていなかった」銘柄。数件なら 9:00:00 ちょうどに
寄らなかっただけ、何十件も出るなら時価問合への反映が間に合っていない。
"""

import argparse

import duckdb
import pandas as pd

HIST = "state/daytrade/history"

SQL = f"""
WITH q AS (
  SELECT CAST(day AS DATE) d, run_id, symbol, opened, from_book, usable, timezone('Asia/Tokyo', recorded_at) jt
  FROM read_parquet('{HIST}/quotes/*.parquet', union_by_name=true) WHERE CAST(day AS DATE) >= CAST(? AS DATE)),
r AS (
  SELECT d, run_id, min(jt) fetched_at FROM q GROUP BY 1, 2
  HAVING hour(min(jt)) = 9 AND minute(min(jt)) <= 2),
o AS (  -- 回の開始 = open_run を書いた時刻 − 所要
  SELECT run_id, min(timezone('Asia/Tokyo', recorded_at) - to_milliseconds(elapsed_ms)) started_at
  FROM read_parquet('{HIST}/open_run/*.parquet', union_by_name=true) GROUP BY 1),
k AS (SELECT *, row_number() OVER (PARTITION BY d ORDER BY fetched_at) k FROM r),
a AS (SELECT q.* FROM q JOIN k USING (d, run_id) WHERE k = 1),
b AS (SELECT q.* FROM q JOIN k USING (d, run_id) WHERE k = 2)
SELECT strftime(k.d, '%m-%d') AS day, k.k AS round, strftime(o.started_at, '%H:%M:%S.%g') AS started, strftime(k.fetched_at, '%H:%M:%S.%g') AS fetched,
       count(*) AS n, sum(q.opened::INT) AS opened, sum(q.from_book::INT) AS from_book, sum(q.usable::INT) AS usable
FROM k JOIN q USING (d, run_id) LEFT JOIN o USING (run_id) WHERE k.k <= 2 GROUP BY ALL ORDER BY 1, 2;
"""

MISSED = SQL.split("SELECT strftime(k.d")[0] + """
SELECT strftime(a.d, '%m-%d') AS day, count(*) AS missed, string_agg(a.symbol, ' ' ORDER BY a.symbol) AS symbols
FROM a JOIN b USING (d, symbol) WHERE b.opened AND NOT a.opened GROUP BY 1 ORDER BY 1
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-16")
    a = ap.parse_args()
    pd.set_option("display.width", 200)
    pd.set_option("display.max_colwidth", 80)
    r = duckdb.sql(SQL, params=[a.since]).df()
    if r.empty:
        print(f"{a.since} 以降に 9:00〜9:02 の回の気配がありません")
        return
    print(r.to_string(index=False))
    w = r.pivot(index="day", columns="round", values="opened")
    if 2 in w.columns:
        print("\n## 1 回目で寄っていた数 ÷ 次の回で寄っていた数（基準 09-18 = 99.0%）")
        print((w[1] / w[2] * 100).round(1).dropna().to_string())
    m = duckdb.sql(MISSED, params=[a.since]).df()
    print("\n## 次の回では寄っていたのに 1 回目で寄っていなかった銘柄")
    print(m.to_string(index=False) if not m.empty else "なし")


if __name__ == "__main__":
    main()
