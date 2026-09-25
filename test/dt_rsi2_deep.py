"""採用候補 O2（RSI(2) ≤ 10 を優先）と A（2 日続落を優先）を、本番の形で測り直す。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md「採用候補 O2 の次の検証」、
      2026-09-jp-daytrade-three-day.md「採用候補 A の次の検証」

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_rsi2_deep.py [--seeds 10] [--i0 10] [--caps 7000000 10000000]

本番の形（test/dt_nscale.py の part_sector_m と同じ部品）:
  気配の誤差（slot 0859、2026-09-11 以降の実測を帯ごとに iid で引く）で見えるギャップを作って並べ直し、
  業種の上限 1、規則 R（p 0.2%・資金 ÷ 7・20 位まで）、日の規則（ショック日 ×1.5、米国小幅高は休み）、
  値を動かすコスト（I0 bp を上位 3 本で較正した √ 則）と FEE。損益は真の寄→引。
  RSI(2) と 2 日続落は前日の引けまでの値なので、気配の誤差の影響を受けない。

事前登録（2026-09-25、結果を見る前に固定。この docstring を commit してから回す）:
  並べ方（業種の上限を掛けたあとの順位 rank に対して）
    base  現行（見えるギャップの gap / vol20）
    O2    rank ≤ 20 の中で RSI(2) ≤ 10 を先に、残りを現行順（O2 の定義は一次の検証のまま）
    A     rank ≤ 20 の中で 2 日続落（前日・前々日とも終値が下げ）を先に
    O2c   連続量の形: その日の候補の中の百分位で rank_pct(G) + 0.5 × rank_pct(RSI(2)) の小さい順（重み 0.5 は固定）
  判定（主）: O2 − base の日次の差（シードで平均した系列）が、OOS（2022〜）で t ≥ 2、かつ IS（2017〜2021）で差 > 0。
             資金 1,000 万・I0 10 bp で判定し、700 万は同じ向きかを見るだけ。
  判定（副）: A と O2c は多重比較を考えて OOS で t ≥ 2.4、かつ IS で差 > 0。
  記述（採否に使わない）: 年ごとの差、RSI(2) の閾値 5・20・30 の感応度、見える順位の帯ごとの対象の超過。
"""

import argparse

import numpy as np
import pandas as pd

from dt_preopen_sim import SNAP_SLOT
from dt_nscale import IS_END, SINCE, alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, seen_ranked, tstat
from dt_oscillator import osc_features
from dt_three_day import lag_features

POOL = 20
W_RSI = 0.5


def with_flags(te):
    days = np.sort(te["d"].unique())
    te = te.copy()
    te["d0"] = te["d"].map(pd.Series(days[:-1], index=days[1:]))
    te = te.merge(osc_features()[["code", "d0", "rsi2"]], on=["code", "d0"], how="left")
    te = te.merge(lag_features()[["code", "d0", "r_d1", "r_d2"]], on=["code", "d0"], how="left")
    te["A"] = (te["r_d1"] < 0) & (te["r_d2"] < 0)
    return te


def reorder(g, how, thr=10):
    """見える順位 rank を並べ替えて付け直す。"""
    g = g.copy()
    if how == "base":
        return g
    if how == "O2c":
        pg = g.groupby("d")["rank"].rank(pct=True)
        pr = g.groupby("d")["rsi2"].rank(pct=True).fillna(1.0)
        g["_k"] = pg + W_RSI * pr
        g = g.sort_values(["d", "_k", "rank"], kind="mergesort")
    else:
        flag = (g["rsi2"] <= thr) if how.startswith("O2") else g["A"]
        g["_p"] = np.where(g["rank"] <= POOL, np.where(flag.fillna(False), 0, 1), 2)
        g = g.sort_values(["d", "_p", "rank"], kind="mergesort")
    g["rank"] = g.groupby("d").cumcount() + 1
    return g


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    ap.add_argument("--i0", type=float, default=10.0)
    ap.add_argument("--caps", type=float, nargs="+", default=[1e7, 7e6])
    ap.add_argument("--slot", default=SNAP_SLOT)
    a = ap.parse_args()
    from dt_preopen_sim import error_pools

    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = with_flags(te[te["d"] >= SINCE])
    pools = error_pools(a.slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    variants = ["base", "O2", "A", "O2c", "O2@5", "O2@20", "O2@30"]
    acc, bands = {}, []
    for seed in range(a.seeds):
        g0 = seen_ranked(te, pools, seed)
        kappa = calib_kappa(g0, a.i0 * 1e-4)
        for v in variants:
            how, thr = (v.split("@")[0], int(v.split("@")[1])) if "@" in v else (v, 10)
            g = reorder(g0, how, thr)
            days = [(d, x) for d, x in g.groupby("d") if not rules.loc[d, "skip"]]
            for C in a.caps:
                s = pd.Series({d: pnl_day(x, *alloc_rule(x, 0.002, 20, C * rules.loc[d, "mult"], k=7), kappa) for d, x in days})
                acc.setdefault((v, C), []).append(s.reindex(alld).fillna(0.0))
        # 記述: 見える順位の帯ごとに、RSI(2) ≤ 10 の銘柄の寄→引の超過（その日の同じ帯の平均との差、bp）
        b = g0[g0["rank"] <= 40].copy()
        b["band"] = pd.cut(b["rank"], [0, 3, 10, 20, 40], labels=["1-3", "4-10", "11-20", "21-40"])
        b["ex"] = (b["y_raw"] - b.groupby(["d", "band"], observed=True)["y_raw"].transform("mean")) * 1e4
        bands.append(b[b["rsi2"] <= 10].groupby("band", observed=True)["ex"].agg(["mean", "size"]))
        print(f"seed {seed}", flush=True)

    per = {"IS 2017〜2021": alld <= IS_END, "OOS 2022〜": alld > IS_END, "2023〜": alld >= "2023-01-01"}
    for C in a.caps:
        base = pd.concat(acc[("base", C)], axis=1).mean(axis=1)
        print(f"\n## 資金 {C / 1e4:.0f} 万・I0 {a.i0:.0f} bp・{a.seeds} シード（日次の円、差はシード平均の系列で t）")
        print("| 並べ方 | " + " | ".join(f"{p} 円/日（base との差, t）" for p in per) + " | OOS 年率・最大DD |")
        print("|---|" + "---|" * (len(per) + 1))
        for v in variants:
            s = pd.concat(acc[(v, C)], axis=1).mean(axis=1)
            cells = [f"{s[m].mean():,.0f}" + ("" if v == "base" else f"（{(s - base)[m].mean():+,.0f}, {tstat((s - base)[m]):+.2f}）")
                     for m in per.values()]
            oos = alld > IS_END
            ann = np.mean([x[oos].mean() * 245 / C * 100 for x in acc[(v, C)]])
            dd = np.mean([max_dd(x[oos].values) / C * 100 for x in acc[(v, C)]])
            print(f"| {v} | " + " | ".join(cells) + f" | {ann:.1f}%・{dd:.1f}% |")
        print("\n年ごとの差（円/日、base との差）: " + "、".join(
            f"{v} " + " ".join(f"{y}:{(pd.concat(acc[(v, C)], axis=1).mean(axis=1) - base)[alld.year == y].mean():+,.0f}"
                              for y in sorted(set(alld.year))) for v in ("O2", "A", "O2c")))
    bt = pd.concat(bands).groupby(level=0)
    print("\n記述: 見える順位の帯ごとの RSI(2) ≤ 10 の超過（bp、全シードの平均、行数）: "
          + "、".join(f"{k} {x['mean'].mean():+.1f}（{int(x['size'].mean())}）" for k, x in bt))


if __name__ == "__main__":
    main()
