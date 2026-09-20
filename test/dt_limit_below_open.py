"""ロングを「始値で買う」代わりに「始値より x% 下の指値で待つ」と成績はどう変わるか（日足の安値で測る）。

問い（2026-09-20、ユーザ）: 始値がその日の安値になることは少ないはず。寄付きより安くなった所を狙う方が安全では。
時刻で待つ形（9:05 に建てる）は 2026-09-jp-daytrade-minute-entry で優位が消えると分かっている。値段で待つ形は未検証。

事前登録（結果を見る前に決めた）:
- 選定は全案で同じ: gap_vol 上位 3・業種 1 銘柄・逆ボラ配分（dt_wf_target と同じ）、2019-09〜、12 月と米国小幅高の日は休む（本番の形）
- 基準 M: 始値で買い、引けで売る（寄成の形）。流動性別コスト
- 案 Lx: 始値 ×(1−x) に指値（x = 0.3 / 0.5 / 1.0 / 1.5 / 2.0%）。安値 < 指値（触れただけは約定としない）の銘柄だけ指値で約定、引けで売る。
  約定しない枠は 0（資金は寝る）。コストは基準と同じ。指値は建値の滑りが無いぶん有利なので、コスト 0 の上限も併記する
- 採用の目安: 同コストで基準を日次の対応差 t ≥ 2 で上回る。コスト 0 の上限でも届かなければ見送り
- 割り引く点: 選定は実際の始値で並べる（気配の誤差なし、全案共通）。日足なので呼値の丸めと約定の順番待ちは見ない（指値側に甘い）

  test/.venv/bin/python test/dt_limit_below_open.py
"""

import duckdb
import numpy as np
import pandas as pd

from dt_wf_target import CAND, evaluate, liq_cost_bp

BARS = "data/jquants/equities_bars_daily/*.parquet"
US = "test/out/mb_panel.parquet"
SINCE = "2019-09-01"
XS = (0.3, 0.5, 1.0, 1.5, 2.0)


def tstat(x):
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))


def main():
    df = pd.read_parquet(CAND)
    df = df[(df["d"] >= SINCE) & (df["d"].dt.month != 12)].copy()
    us = pd.read_parquet(US)[["date", "spx_ret1", "vix"]].rename(columns={"date": "d"})
    us["us_low"] = (us["spx_ret1"] >= 0) & (us["spx_ret1"] < 0.01) & (us["vix"].isna() | (us["vix"] <= 24))
    df = df[~df["d"].isin(us.loc[us["us_low"], "d"])]
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    df = df.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    df["rule_rank"] = df.groupby("d").cumcount() + 1

    p = evaluate(df, -df["key_sort"].values, "gap_vol", 0).merge(df[["d", "code", "o", "c"]], on=["d", "code"])
    low = duckdb.sql(f"""SELECT CAST(Date AS DATE) d, CAST(Code AS VARCHAR) code, TRY_CAST(L AS DOUBLE) l
                         FROM read_parquet('{BARS}', union_by_name=true) WHERE CAST(Date AS DATE) >= DATE '{SINCE}'""").df()
    low["d"] = pd.to_datetime(low["d"])
    p = p.merge(low, on=["d", "code"], how="left")
    p = p[p["l"] > 0].copy()
    p["cost"] = liq_cost_bp(p["turnover_med"].values) / 1e4
    p["dip"] = p["l"] / p["o"] - 1
    days = pd.Index(sorted(p["d"].unique()))

    print(f"{days[0]:%Y-%m-%d}〜{days[-1]:%Y-%m-%d}、{len(days)} 日、選定 {len(p)} 件")
    print("\n## 前提の確認: 選んだ銘柄は始値からどこまで下げるか（安値 ÷ 始値 − 1）")
    print(f"始値 = 安値 {np.mean(p['dip'] == 0) * 100:.1f}%、中央値 {p['dip'].median() * 100:.2f}%、" +
          "、".join(f"{x}% 以上下げる {np.mean(p['dip'] < -x / 100) * 100:.0f}%" for x in XS))

    base = (p["w"] * (p["y_raw"] - p["cost"])).groupby(p["d"]).sum().reindex(days, fill_value=0.0)
    print("\n## 日次の成績（bp/日）")
    print("| 建て方 | 約定率 | bp/日 | t | 基準との差 | 差の t | コスト 0 の上限 bp/日 | 差の t |")
    print("|---|---|---|---|---|---|---|---|")
    print(f"| M 始値で買う | 100% | {base.mean() * 1e4:+.2f} | {tstat(base):.2f} | — | — | — | — |")
    for x in XS:
        lim = p["o"] * (1 - x / 100)
        fill = p["l"] < lim
        r = np.where(fill, p["c"] / lim - 1, 0.0)
        out = []
        for cost in (p["cost"], 0.0):
            v = (p["w"] * np.where(fill, r - cost, 0.0)).groupby(p["d"]).sum().reindex(days, fill_value=0.0)
            out.append((v.mean() * 1e4, tstat(v), tstat(v - base)))
        print(f"| L{x} 始値 −{x}% に指値 | {fill.mean() * 100:.0f}% | {out[0][0]:+.2f} | {out[0][1]:.2f} | {out[0][0] - base.mean() * 1e4:+.2f} | {out[0][2]:.2f} | {out[1][0]:+.2f} | {out[1][2]:.2f} |")

    print("\n## 買えた銘柄と買えなかった銘柄（1 件あたり、コスト前、%）")
    print("| 指値 | 買えた: 件数 | 始値→引け | 指値→引け | 買えなかった: 件数 | 始値→引け（取り逃し） |")
    print("|---|---|---|---|---|---|")
    for x in XS:
        lim = p["o"] * (1 - x / 100)
        f = p["l"] < lim
        print(f"| −{x}% | {f.sum()} | {p.loc[f, 'y_raw'].mean() * 100:+.2f} | {(p.loc[f, 'c'] / lim[f] - 1).mean() * 100:+.2f} | {(~f).sum()} | {p.loc[~f, 'y_raw'].mean() * 100:+.2f} |")


if __name__ == "__main__":
    main()
