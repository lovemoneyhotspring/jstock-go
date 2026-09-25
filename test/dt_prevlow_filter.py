"""ロング: 上位に入っても、前日安値より上で寄る銘柄は負けるのではないか（除外の仮説）。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_prevlow_filter.py [--top 10]

仮説（ユーザ、2026-09-25）: 下げトレンドの銘柄が前日の後場に急に戻すと、終値からはギャップダウンでも
前日安値より上で寄る。こういう銘柄は売り込まれて負ける。

事前登録（結果を見る前に固定）:
  - 母集団・期間・指標は test/dt_prevlow_gap.py と同じ（真の始値・上位 N 本・等金額・流動性コスト込み）
  - 除外の形（外した分は次点で埋め、毎日 N 本を保つ）
      F1 始値 ≥ 前日安値
      F2 F1 かつ下げトレンド（ret20 < 0）
      F3 F2 かつ前日の戻り（前日終値 / 前日安値 − 1）≥ vol20（日中に 1σ 以上戻した）
  - 採否: 現行 G との日次の差が全期間で t ≥ 2、かつ 2016〜2022・2023〜2026 の両方で差 > 0
  - 補助: 現行の上位 N 本の中で、除外の対象になる銘柄の「その日の上位平均からの超過」を日に畳んで出す
前日の後場は日足では分からないので、「終値が安値からどれだけ戻したか」で代える。
"""

import argparse

import numpy as np
import pandas as pd

from dt_prevlow_gap import CAND, VOL_FLOOR, prev_range, tstat
from dt_wf_target import liq_cost_bp


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    a = ap.parse_args()

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(prev_range(), on=["code", "d0"], how="inner")
    v = np.maximum(c["vol20"], VOL_FLOOR)
    c["above_low"] = c["o"] >= c["prev_close"] * c["l_c"]
    c["rebound"] = 1 / c["l_c"] - 1
    c["F1"] = c["above_low"]
    c["F2"] = c["F1"] & (c["ret20"] < 0)
    c["F3"] = c["F2"] & (c["rebound"] >= v)
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    c = c.sort_values(["d", "key_sort", "code"], kind="mergesort")

    base = c.groupby("d").head(a.top)
    daily = {"G": base.groupby("d")["ret"].mean()}
    for f in ("F1", "F2", "F3"):
        daily[f] = c[~c[f]].groupby("d").head(a.top).groupby("d")["ret"].mean()
    daily = pd.DataFrame(daily)
    yr = daily.index.year
    periods = {"全期間": np.ones(len(daily), bool), "2016〜2022": yr <= 2022, "2023〜2026": yr >= 2023}

    print(f"上位 {a.top} 本に占める対象: " + "、".join(f"{f} {base[f].mean():.1%}" for f in ("F1", "F2", "F3")))
    print(f"\n除外して次点で埋める（bp/日、G との差, t）")
    print("| 形 | " + " | ".join(periods) + " |")
    print("|---|" + "---|" * len(periods))
    for k in daily:
        cells = []
        for m in periods.values():
            d = daily[m]
            diff = d[k] - d["G"]
            cells.append(f"{d[k].mean():+.2f}" + ("" if k == "G" else f"（{diff.mean():+.2f}, {tstat(diff):+.2f}）"))
        print(f"| {k} | " + " | ".join(cells) + " |")

    # 補助: 現行の上位の中で、対象の銘柄が上位平均をどれだけ上回る／下回るか（日に畳む）
    base = base.assign(ex=base["ret"] - base.groupby("d")["ret"].transform("mean"))
    print(f"\n現行の上位 {a.top} 本の中での超過（bp、日に畳んだ平均と t、対象が 1 本以上ある日）")
    for f in ("F1", "F2", "F3"):
        for flag, name in ((True, "対象"), (False, "対象外")):
            s = base[base[f] == flag].groupby("d")["ex"].mean()
            by = {p: s[s.index.year <= 2022] if p == "2016〜2022" else s[s.index.year >= 2023] if p == "2023〜2026" else s
                  for p in periods}
            print(f"  {f} {name}: " + "、".join(f"{p} {x.mean():+.1f}（t {tstat(x):+.2f}, {len(x)} 日）" for p, x in by.items()))


if __name__ == "__main__":
    main()
