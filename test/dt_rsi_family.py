"""RSI の派生（定番の設定のまま）で、採用候補 O2（RSI(2) ≤ 10 を優先）より良くなるか。本番の形で測る。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md（O2 の深掘り）

  PYTHONPATH=test test/.venv/bin/python test/dt_rsi_family.py [--seeds 10] [--i0 10] [--cap 10000000]

事前登録（2026-09-25、結果を見る前に固定。この docstring を commit してから回す）:
  位置づけ: 同じ候補表・期間での 5 本目の探索。採否は決めない。前向きの記録で O2 と一緒に取る指標を選ぶ材料にする。
  指標は定番の設定のまま（調整しない）。すべて前日の引けまでの値。
    CRSI  Connors RSI(3, 2, 100) = (RSI(3) + 連騰・連敗日数の RSI(2) + 前日騰落の 100 日の百分位) / 3 ≤ 10
    CUM   累積 RSI(2): 前日と前々日の RSI(2) の和 < 35
    SRSI  ストキャス RSI(14, 14): (RSI14 − 14 日の最小) / (14 日の最大 − 最小) ≤ 0.2
    O2T   RSI(2) ≤ 10 かつ前日終値 > 200 日線（Connors の元の形）
  使い方は O2 と同じ: 業種の上限を掛けたあとの rank ≤ 20 の中で対象を先に、残りを現行順。
  本番の形は test/dt_rsi2_deep.py と同じ（気配の誤差・業種 1・規則 R÷7・日の規則・値を動かすコスト）。
  記述の目安: base との差が OOS（2022〜）で t ≥ 2.5 かつ IS で差 > 0 なら「O2 と並べて記録する価値あり」。
             O2 との差（同じシード）も出すが、目安には使わない。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_nscale import IS_END, SINCE, alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, seen_ranked, tstat, tstat_err
from dt_oscillator import rsi
from dt_rsi2_deep import POOL

FLAGS = ["O2", "CRSI", "CUM", "SRSI", "O2T"]


def family_features():
    cols = ["Code", "Date", "C", "AdjFactor"]
    b = pd.concat([pd.read_parquet(f, columns=cols) for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))],
                  ignore_index=True)
    for k in cols[2:]:
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[b["C"] > 0].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    b["C"] = b["C"] * later
    g = b.groupby("code")["C"]
    out = pd.DataFrame({"code": b["code"], "d0": b["d0"]})
    out["rsi2"] = g.transform(lambda x: rsi(x, 2))
    rsi3 = g.transform(lambda x: rsi(x, 3))
    # 連騰・連敗日数（上げが続けば +1, +2…、下げが続けば −1, −2…、変わらずは 0）
    sgn = np.sign(g.diff()).fillna(0.0)
    run = (sgn != sgn.groupby(b["code"]).shift()).groupby(b["code"]).cumsum()
    streak = sgn * (sgn.groupby([b["code"], run]).cumcount() + 1)
    streak_rsi = streak.groupby(b["code"]).transform(lambda x: rsi(x, 2))
    roc = g.pct_change()
    prank = roc.groupby(b["code"]).transform(lambda x: x.rolling(100).rank(pct=True) * 100)
    out["crsi"] = (rsi3 + streak_rsi + prank) / 3
    out["cum2"] = out["rsi2"] + out.groupby("code")["rsi2"].shift(1)
    r14 = g.transform(lambda x: rsi(x, 14))
    lo, hi = (r14.groupby(b["code"]).transform(lambda x: x.rolling(14).min()),
              r14.groupby(b["code"]).transform(lambda x: x.rolling(14).max()))
    out["srsi"] = np.where(hi > lo, (r14 - lo) / (hi - lo), np.nan)
    out["above200"] = b["C"] > g.transform(lambda x: x.rolling(200).mean())
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    ap.add_argument("--i0", type=float, default=10.0)
    ap.add_argument("--cap", type=float, default=1e7)
    ap.add_argument("--slot", default="0859")
    a = ap.parse_args()
    from dt_preopen_sim import error_pools

    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    days = np.sort(te["d"].unique())
    te["d0"] = te["d"].map(pd.Series(days[:-1], index=days[1:]))
    te = te.merge(family_features(), on=["code", "d0"], how="left")
    te["O2"] = te["rsi2"] <= 10
    te["CRSI"] = te["crsi"] <= 10
    te["CUM"] = te["cum2"] < 35
    te["SRSI"] = te["srsi"] <= 0.2
    te["O2T"] = te["O2"] & te["above200"]
    pools = error_pools(a.slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    C = a.cap
    acc, share = {}, {}
    for seed in range(a.seeds):
        g0 = seen_ranked(te, pools, seed)
        kappa = calib_kappa(g0, a.i0 * 1e-4)
        for v in ["base"] + FLAGS:
            g = g0
            if v != "base":
                g = g0.assign(_p=np.where(g0["rank"] <= POOL, np.where(g0[v].fillna(False), 0, 1), 2))
                g = g.sort_values(["d", "_p", "rank"], kind="mergesort")
                g["rank"] = g.groupby("d").cumcount() + 1
                if seed == 0:
                    share[v] = g0.loc[g0["rank"] <= 10, v].fillna(False).mean()
            dd = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
            s = pd.Series({d: pnl_day(x, *alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"], k=7), kappa) for d, x in dd})
            acc.setdefault(v, []).append(s.reindex(alld).fillna(0.0))
        print(f"seed {seed}", flush=True)

    per = {"IS 2017〜2021": alld <= IS_END, "OOS 2022〜": alld > IS_END, "2023〜": alld >= "2023-01-01"}
    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    print(f"\n上位 10 本に占める対象: " + "、".join(f"{v} {s:.1%}" for v, s in share.items()))
    print(f"\n## 資金 {C / 1e4:.0f} 万・I0 {a.i0:.0f} bp・{a.seeds} シード（円/日。base との差, t ／ O2 との差, t）")
    print("| 指標 | " + " | ".join(per) + " | OOS 年率・最大DD |")
    print("|---|" + "---|" * (len(per) + 1))
    oos = alld > IS_END
    for v in ["base"] + FLAGS:
        cells = []
        for m in per.values():
            c = f"{mean[v][m].mean():,.0f}"
            if v != "base":
                db, do = mean[v] - mean["base"], mean[v] - mean["O2"]
                c += f"（{db[m].mean():+,.0f}, {tstat(db[m]):+.2f}, 誤差込み {tstat_err(acc[v], acc['base'], m):+.2f}"
                c += "）" if v == "O2" else f" ／ {do[m].mean():+,.0f}, {tstat(do[m]):+.2f}）"
            cells.append(c)
        ann = np.mean([x[oos].mean() * 245 / C * 100 for x in acc[v]])
        dd = np.mean([max_dd(x[oos].values) / C * 100 for x in acc[v]])
        print(f"| {v} | " + " | ".join(cells) + f" | {ann:.1f}%・{dd:.1f}% |")


if __name__ == "__main__":
    main()
