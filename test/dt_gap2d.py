"""ロング: 前々日の終値とのギャップ（2 日ギャップ）で並べたら銘柄選定は良くなるか。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_gap2d.py [--top 10]

2 日ギャップ gap2 = 始値 / 前々日終値 − 1 = (1 + gap)(1 + 前日の騰落) − 1

事前登録（結果を見る前に固定）:
  母集団・期間・指標は test/dt_prevlow_gap.py と同じ（真の始値・上位 N 本・等金額・流動性コスト込み）。
  並べ方（小さい順 = 深い順）
    G     現行 gap / max(vol20, 2%)
    G0    参考: 素の gap
    H     素の gap2
    HV    gap2 / max(vol20, 2%)（√2 を掛けても順位は同じなので 1 本）
    HG    G と HV の日内順位の平均
  採否: G との日次の差が全期間で t ≥ 2.5（3 本を試すので 2 より厳しく）、かつ 2016〜2022・2023〜2026 の両方で差 > 0。
  補助（採否に使わない）: 候補全体の横断回帰で HV が G を知った上で追加の情報を持つか（Fama-MacBeth）。
"""

import argparse

import numpy as np
import pandas as pd

from dt_prevlow_gap import CAND, VOL_FLOOR, tstat
from dt_three_day import lag_features
from dt_wf_target import liq_cost_bp


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    a = ap.parse_args()

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(lag_features()[["code", "d0", "r_d1"]], on=["code", "d0"], how="inner").dropna(subset=["r_d1"])
    v = np.maximum(c["vol20"], VOL_FLOOR)
    c["G"] = c["key_sort"]
    c["G0"] = c["gap"]
    c["H"] = (1 + c["gap"]) * (1 + c["r_d1"]) - 1
    c["HV"] = c["H"] / v
    c["HG"] = c.groupby("d")["G"].rank() + c.groupby("d")["HV"].rank()
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    g = c.groupby("d")
    print(f"{len(c):,} 行 / {c['d'].nunique():,} 日。G と HV の日内順位相関の中央値 "
          f"{g.apply(lambda x: x['G'].corr(x['HV'], method='spearman')).median():.2f}、"
          f"上位 {a.top} 本の重なり {g.apply(lambda x: len(set(x.nsmallest(a.top, 'G').code) & set(x.nsmallest(a.top, 'HV').code))).mean():.1f} 本")

    daily = pd.DataFrame({k: c.sort_values(["d", k, "code"], kind="mergesort").groupby("d").head(a.top)
                          .groupby("d")["ret"].mean() for k in ("G", "G0", "H", "HV", "HG")})
    yr = daily.index.year
    periods = {"全期間": np.ones(len(daily), bool), "2016〜2022": yr <= 2022, "2023〜2026": yr >= 2023}
    print(f"\n上位 {a.top} 本（bp/日、G との差, t）")
    print("| 並べ方 | " + " | ".join(periods) + " |")
    print("|---|" + "---|" * len(periods))
    for k in daily:
        cells = []
        for m in periods.values():
            d = daily[m]
            diff = d[k] - d["G"]
            cells.append(f"{d[k].mean():+.2f}" + ("" if k == "G" else f"（{diff.mean():+.2f}, {tstat(diff):+.2f}）"))
        print(f"| {k} | " + " | ".join(cells) + " |")

    def fm(x):
        if len(x) < 30:
            return None
        z = lambda s: (s - s.mean()) / (s.std() or 1)
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        both = np.linalg.lstsq(np.column_stack([np.ones(len(x)), z(x["G"]), z(x["HV"])]), y, rcond=None)[0]
        return pd.Series({"G（HV と同時）": both[1], "HV（G と同時）": both[2], "HV 単独": np.polyfit(z(x["HV"]), y, 1)[0],
                          "G 単独": np.polyfit(z(x["G"]), y, 1)[0]})
    f = c.groupby("d")[["G", "HV", "y_raw"]].apply(fm).dropna()
    print("\n補助: 候補全体の横断回帰（1σ あたり bp、負 = 深いほど上がる）")
    for k in f:
        s = f[k]
        print(f"  {k}: 全期間 {s.mean():+.2f}（t {tstat(s):+.2f}）、2016〜2022 {s[s.index.year <= 2022].mean():+.2f}"
              f"（t {tstat(s[s.index.year <= 2022]):+.2f}）、2023〜2026 {s[s.index.year >= 2023].mean():+.2f}"
              f"（t {tstat(s[s.index.year >= 2023]):+.2f}）")


if __name__ == "__main__":
    main()
