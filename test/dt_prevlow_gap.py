"""ロングの並べ方: 前日終値とのギャップの代わりに、前日安値とのギャップで並べたら良くなるか（一次の比較）。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  test/.venv/bin/python test/dt_prevlow_gap.py [--top 10]

真の始値・同じ候補表・毎日上位 N 本の等金額で、並べ方だけを替えて日次の損益を対応のある形で比べる。
  G   現行: gap / max(vol20, 2%)（selection.RankKey）
  L   前日安値比: (o / 前日安値 - 1) / max(vol20, 2%)
  P   前日値幅の中の位置: (o - 前日安値) / (前日高値 - 前日安値)（小さいほど深い）
  GL  G と L の順位の平均
加えて、日ごとの横断回帰 y = a + b1·z(G) + b2·z(L) の b2（Fama-MacBeth、日に畳んだ t）で
「前日終値のギャップを知った上で、安値のギャップに追加の情報があるか」を見る。
前日の安値・高値は日足の生値の L/C・H/C を前日終値に掛けて作る（分割の調整に左右されない）。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_wf_target import liq_cost_bp

CAND = "test/out/dt_candidates.parquet"
VOL_FLOOR = 0.02


def prev_range():
    """(code, d) → 前日の L/C・H/C。d は「翌営業日」＝候補表の日付。"""
    parts = []
    for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet")):
        b = pd.read_parquet(f, columns=["Code", "Date", "H", "L", "C"])
        parts.append(b)
    b = pd.concat(parts, ignore_index=True)
    for k in ("H", "L", "C"):
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[(b["C"] > 0) & (b["L"] > 0) & (b["H"] > 0)]
    b["d0"] = pd.to_datetime(b["Date"])
    b["l_c"] = b["L"] / b["C"]
    b["h_c"] = b["H"] / b["C"]
    return b.rename(columns={"Code": "code"})[["code", "d0", "l_c", "h_c"]]


def tstat(x):
    x = np.asarray(x, dtype=float)
    x = x[~np.isnan(x)]
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    a = ap.parse_args()

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()  # vol20 の無い銘柄は現行でも末尾なので外す
    days = np.sort(c["d"].unique())
    prev_day = pd.Series(days[:-1], index=days[1:])
    c["d0"] = c["d"].map(prev_day)  # 候補表の前の営業日（候補の無い日は無いので営業日と一致する）
    c = c.merge(prev_range(), on=["code", "d0"], how="inner")
    pl, ph = c["prev_close"] * c["l_c"], c["prev_close"] * c["h_c"]
    v = np.maximum(c["vol20"], VOL_FLOOR)
    c["G"] = c["key_sort"]
    c["L"] = (c["o"] / pl - 1) / v
    c["P"] = np.where(ph > pl, (c["o"] - pl) / (ph - pl), np.nan)
    c["P"] = c["P"].fillna(c["P"].median())
    g = c.groupby("d")
    c["GL"] = g["G"].rank() + g["L"].rank()
    c["ret"] = c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4
    print(f"{len(c):,} 行 / {c['d'].nunique():,} 日。前日安値より下で寄った候補 {np.mean(c['o'] < pl):.1%}、"
          f"G と L の日内順位相関の中央値 {g.apply(lambda x: x['G'].corr(x['L'], method='spearman')).median():.2f}")

    daily = {}
    for k in ("G", "L", "P", "GL"):
        top = c.sort_values(["d", k, "code"], kind="mergesort").groupby("d").head(a.top)
        daily[k] = top.groupby("d")["ret"].mean() * 1e4
    daily = pd.DataFrame(daily)
    yr = daily.index.year
    periods = {"全期間": slice(None), "2016〜2022": yr <= 2022, "2023〜2026": yr >= 2023}
    print(f"\n上位 {a.top} 本・等金額・流動性コスト込み（bp/日）")
    print("| 並べ方 | " + " | ".join(f"{p} bp（G との差, t）" for p in periods) + " |")
    print("|---|" + "---|" * len(periods))
    for k in daily:
        cells = []
        for m in periods.values():
            d = daily[m] if not isinstance(m, slice) else daily
            diff = d[k] - d["G"]
            cells.append(f"{d[k].mean():+.2f}（{diff.mean():+.2f}, {tstat(diff) if k != 'G' else float('nan'):+.2f}）")
        print(f"| {k} | " + " | ".join(cells) + " |")

    # Fama-MacBeth: 日ごとに z(G)・z(L) で y_raw を回帰
    def fm(x):
        if len(x) < 20:
            return pd.Series({"bG": np.nan, "bL": np.nan, "bL_only": np.nan})
        z = lambda s: (s - s.mean()) / s.std()
        X = np.column_stack([np.ones(len(x)), z(x["G"]), z(x["L"])])
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        b = np.linalg.lstsq(X, y, rcond=None)[0]
        bl = np.polyfit(z(x["L"]), y, 1)[0]
        return pd.Series({"bG": b[1], "bL": b[2], "bL_only": bl})
    f = c.groupby("d")[["G", "L", "y_raw"]].apply(fm).dropna()
    print("\n日次の横断回帰（1σ あたり bp、負 = 深いほど上がる）")
    for col, name in (("bG", "G（L と同時）"), ("bL", "L（G と同時）"), ("bL_only", "L 単独")):
        print(f"  {name}: {f[col].mean():+.2f} bp（t {tstat(f[col]):+.2f}）")


if __name__ == "__main__":
    main()
