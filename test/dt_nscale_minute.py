"""daytrade ロング: 寄りの板に入りきらない金額を、寄りの後の連続売買（9:01〜9:10）で足す案の測定。

根拠: vault 20-research/2026-09-jp-daytrade-nscale.md「次の検証 4 ③」

  test/.venv/bin/python test/dt_nscale_minute.py

比べるもの（どちらも「寄りで買う N=3 の外に足す 1 円」の効き）:
  - 1〜3 位を t 分の VWAP で買い増す: net = 引け ÷ VWAP_t − 1 − コスト
  - 4〜10 位・11〜20 位を寄りで買う: net = 引け ÷ 始値 − 1 − コスト
容量は 9:00 の分足の売買代金（板寄せ＋9:00 台の連続売買）と、t 分までの累積売買代金。
分足は 2024-09〜（data/jquants/equities_bars_minute）。順位は候補表（真の始値のギャップ）で付ける。
"""

import duckdb
import numpy as np
import pandas as pd

from dt_nscale import COST, load, tstat

MIN = "data/jquants/equities_bars_minute/*.parquet"
TIMES = ["09:00", "09:01", "09:02", "09:03", "09:05", "09:10"]


def main():
    c = load()
    c = c[(c["d"] >= "2024-09-02") & (c["rank"] <= 20)].copy()
    keys = c[["d", "code"]].rename(columns={"d": "kd", "code": "kcode"})
    keys["kd"] = keys["kd"].dt.strftime("%Y-%m-%d")
    con = duckdb.connect()
    con.register("keys", keys)
    m = con.sql(f"""
        SELECT CAST(Date AS VARCHAR) d, CAST(Code AS VARCHAR) code, Time t, Va va, Vo vo
        FROM read_parquet('{MIN}', union_by_name=true) m
        JOIN keys k ON k.kd = CAST(m.Date AS VARCHAR) AND k.kcode = CAST(m.Code AS VARCHAR)
        WHERE Time <= '09:10'
    """).df()
    m["vwap"] = m["va"] / m["vo"].replace(0, np.nan)
    m["d"] = pd.to_datetime(m["d"])
    c = c.merge(m[m["t"] == "09:00"][["d", "code", "va"]].rename(columns={"va": "va0900"}), on=["d", "code"], how="left")
    c["rb"] = pd.cut(c["rank"], [0, 3, 10, 20], labels=["1-3", "4-10", "11-20"])
    print(f"分足の期間: {c['d'].min():%Y-%m-%d}〜{c['d'].max():%Y-%m-%d}、{c['d'].nunique()} 日")
    print("\n寄りで買う（net bp、日次平均）と 9:00 の分足の売買代金（中央値）")
    for rb, g in c.groupby("rb", observed=True):
        s = g.groupby("d")["net"].mean()
        print(f"  {rb:5s}: {s.mean():6.1f} bp (t {tstat(s):5.2f})  9:00 の代金 中央値 {g['va0900'].median()/1e8:5.2f} 億")
    top = c[c["rank"] <= 3]
    print("\n1〜3 位を t 分の VWAP で買い増す（net bp、日次平均）と、9:01 から t 分までの累積代金（中央値）")
    for t in TIMES[1:]:
        x = m[m["t"] == t][["d", "code", "vwap"]]
        cum = m[(m["t"] > "09:00") & (m["t"] <= t)].groupby(["d", "code"])["va"].sum().rename("cumva").reset_index()
        g = top.merge(x, on=["d", "code"]).merge(cum, on=["d", "code"], how="left")
        g["net_t"] = (g["c"] / g["vwap"] - 1 - COST) * 1e4
        s = g.groupby("d")["net_t"].mean()
        print(f"  {t}: {s.mean():6.1f} bp (t {tstat(s):5.2f}, {len(g)} 件、その分に約定のある割合 {len(g)/len(top):.0%})"
              f"  累積代金 中央値 {g['cumva'].median()/1e8:5.2f} 億")


if __name__ == "__main__":
    main()
