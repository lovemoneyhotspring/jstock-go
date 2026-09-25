"""ロング: 銘柄個別のトレンドを加味したら銘柄選定は良くなるか（5 案）。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_trend.py [--top 10]

仮説（ユーザ、2026-09-25）: 銘柄個別のトレンドを加味したほうが成績が上がる。

事前登録（結果を見る前に固定。値はすべて前日の引けまで、ただし T4 だけ当日の始値を使う）:
  母集団・指標は test/dt_prevlow_gap.py と同じ（真の始値・上位 N 本・等金額・流動性コスト込み）。
  期間は 250 日の履歴が揃う 2017-09 以降（全案と G を同じ日で比べる）。前後は 2017-09〜2022 / 2023〜2026。
  ◎ 優先 = 現行の上位 2N の中で対象を先に取り、残りを現行順で埋める。× 除外 = 対象を外して次点で埋める。
    T1 × 落ちるナイフ: 前日終値 < 200 日移動平均、かつ 200 日移動平均が 20 日前より下
    T2 ◎ 上昇トレンドの押し目: 前日終値 > 75 日移動平均、かつ 25 日移動平均 > 75 日移動平均
    T3 ◎ 52 週高値に近い: 前日終値 ≥ 250 日の終値の最高値 × 0.9
    T4 ◎ 25 日移動平均からの乖離が深い（売られすぎ）: 始値 / 25 日移動平均 − 1 ≤ −10%
    T5 ◎ 中期の上昇（12-1 モメンタム）: d-250〜d-21 の騰落が、その日の候補の上位 1/3
  採否: G との日次の差が全期間で t ≥ 2.6（5 本を試すので厳しく）、かつ前後の両方で差 > 0。
  補助（採否に使わない）: 上位 N 本の中での対象の超過（日に畳む）、候補全体の横断回帰（Fama-MacBeth、G と同時）。
過去の値は生値に AdjFactor の後ろ向きの積を掛けた調整済みの終値で作る。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_prevlow_gap import CAND, tstat
from dt_wf_target import liq_cost_bp

START = pd.Timestamp("2017-09-01")


def trend_features():
    """(code, d0 = 前日) → 前日の引けまでのトレンドの量。"""
    b = pd.concat([pd.read_parquet(f, columns=["Code", "Date", "C", "AdjFactor"])
                   for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))], ignore_index=True)
    for k in ("C", "AdjFactor"):
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[b["C"] > 0].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    b["A"] = b["C"] * later
    g = b.groupby("code")["A"]
    out = pd.DataFrame({"code": b["code"], "d0": b["d0"], "cl": b["A"]})
    out["ma25"] = g.transform(lambda x: x.rolling(25).mean())
    out["ma75"] = g.transform(lambda x: x.rolling(75).mean())
    out["ma200"] = g.transform(lambda x: x.rolling(200).mean())
    out["ma200_20"] = out.groupby("code")["ma200"].shift(20)
    out["hi250"] = g.transform(lambda x: x.rolling(250).max())
    out["mom"] = g.shift(20) / g.shift(250) - 1
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    a = ap.parse_args()
    n = a.top

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(trend_features(), on=["code", "d0"], how="inner")
    c = c[c["d"] >= START].dropna(subset=["ma200_20", "hi250", "mom"])
    c["G"] = c["key_sort"]
    c["T1"] = (c["cl"] < c["ma200"]) & (c["ma200"] < c["ma200_20"])
    c["T2"] = (c["cl"] > c["ma75"]) & (c["ma25"] > c["ma75"])
    c["T3"] = c["cl"] >= c["hi250"] * 0.9
    c["dev25"] = c["o"] / c["prev_close"] * c["cl"] / c["ma25"] - 1  # 始値 / 25 日移動平均 − 1（単位を前日終値でつなぐ）
    c["T4"] = c["dev25"] <= -0.10
    c["T5"] = c.groupby("d")["mom"].rank(pct=True) > 2 / 3
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    c = c.sort_values(["d", "G", "code"], kind="mergesort").reset_index(drop=True)
    c["rk"] = c.groupby("d").cumcount()
    flags = {"T1": "×", "T2": "◎", "T3": "◎", "T4": "◎", "T5": "◎"}

    def prefer(flag):
        pool = c[c["rk"] < 2 * n].assign(p=lambda x: (~x[flag]).astype(int))
        return pool.sort_values(["d", "p", "rk"]).groupby("d").head(n).groupby("d")["ret"].mean()

    def exclude(flag):
        return c[~c[flag]].groupby("d").head(n).groupby("d")["ret"].mean()

    base = c[c["rk"] < n]
    daily = pd.DataFrame({"G": base.groupby("d")["ret"].mean(),
                          **{f + m: (exclude(f) if m == "×" else prefer(f)) for f, m in flags.items()}})
    yr = daily.index.year
    periods = {"全期間": np.ones(len(daily), bool), "2017-09〜2022": yr <= 2022, "2023〜2026": yr >= 2023}
    print(f"{len(c):,} 行 / {c['d'].nunique():,} 日")
    print("上位 N 本に占める対象: " + "、".join(f"{f} {base[f].mean():.1%}" for f in flags)
          + "（上位 2N: " + "、".join(f"{f} {c[c.rk < 2 * n][f].mean():.1%}" for f in flags) + "）")
    print(f"\n上位 {n} 本（bp/日、G との差, t）")
    print("| 案 | " + " | ".join(periods) + " |")
    print("|---|" + "---|" * len(periods))
    for k in daily:
        cells = []
        for m in periods.values():
            d = daily[m]
            diff = d[k] - d["G"]
            cells.append(f"{d[k].mean():+.2f}" + ("" if k == "G" else f"（{diff.mean():+.2f}, {tstat(diff):+.2f}）"))
        print(f"| {k} | " + " | ".join(cells) + " |")

    def split(s):
        return {"全期間": s, "〜2022": s[s.index.year <= 2022], "2023〜": s[s.index.year >= 2023]}

    base = base.assign(ex=base["ret"] - base.groupby("d")["ret"].transform("mean"))
    print(f"\n補助: 現行の上位 {n} 本の中での対象の超過（bp、日に畳む）")
    for f in flags:
        s = base[base[f]].groupby("d")["ex"].mean()
        print(f"  {f}: " + "、".join(f"{p} {x.mean():+.1f}（t {tstat(x):+.2f}, {len(x)} 日）" for p, x in split(s).items()))

    c["x_ma200"] = c["cl"] / c["ma200"] - 1
    c["x_ma25_75"] = c["ma25"] / c["ma75"] - 1
    c["x_hi250"] = c["cl"] / c["hi250"] - 1
    feats = ["x_ma200", "x_ma25_75", "x_hi250", "dev25", "mom"]

    def fm(x):
        if len(x) < 30:
            return None
        z = lambda s: (s - s.mean()) / (s.std() or 1)
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        return pd.Series({f: np.linalg.lstsq(np.column_stack([np.ones(len(x)), z(x["G"]), z(x[f])]), y, rcond=None)[0][2]
                          for f in feats})
    r = c.groupby("d")[["G", "y_raw"] + feats].apply(fm).dropna()
    print("\n補助: 候補全体の横断回帰（G と同時、1σ あたり bp。正 = その量が大きいほど当日上がる）")
    for f in feats:
        print(f"  {f}: " + "、".join(f"{p} {s.mean():+.2f}（t {tstat(s):+.2f}）" for p, s in split(r[f]).items()))


if __name__ == "__main__":
    main()
