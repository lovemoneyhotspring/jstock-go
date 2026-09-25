"""ワイルダーの ATR・DMI/ADX（定番の設定のまま）でロングの選定が良くなるか。本番の形で測る。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md（RSI の派生の比較）

  PYTHONPATH=test test/.venv/bin/python test/dt_wilder.py [--seeds 10] [--i0 10] [--cap 10000000]

事前登録（2026-09-25、結果を見る前に固定。この docstring を commit してから回す）:
  位置づけ: 同じ候補表・期間での 6 本目の探索。採否は決めず、前向きの記録で取る指標を選ぶ材料にする。
  指標は定番の設定のまま（期間 14、Wilder の平滑、ADX の閾値 20 / 25）。すべて前日の引けまでの値。
    K_ATR  並べ方の分母を替える: 見えるギャップ / max(ATR(14) / 前日終値, 2%)（現行は / max(vol20, 2%)）
    D1 ◎   ADX(14) < 20（トレンドが弱い＝レンジで逆張りが効く）
    D2 ×   ADX(14) ≥ 25 かつ −DI > +DI（強い下降トレンド）を外す
  ◎ は業種の上限を掛けたあとの rank ≤ 20 の中で対象を先に。× は対象を外して次点で埋める。
  K_ATR は並べ直したうえで業種の上限を掛け直す。
  本番の形は test/dt_rsi2_deep.py と同じ（気配の誤差・業種 1・規則 R÷7・日の規則・値を動かすコスト）。
  参考に SRSI（ストキャス RSI(14,14) ≤ 0.2 優先）も同じシードで並べる（目安には使わない）。
  記述の目安: base との差が OOS（2022〜）で t ≥ 2.5 かつ IS で差 > 0 なら「前向きに記録する価値あり」。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_preopen_sim import SNAP_SLOT
from dt_nscale import COST, IS_END, SINCE, alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat
from dt_rsi2_deep import POOL
from dt_rsi_family import family_features

VOL_FLOOR = 0.02


def wilder(x, n):
    return x.ewm(alpha=1 / n, adjust=False, min_periods=n).mean()


def wilder_features():
    cols = ["Code", "Date", "H", "L", "C", "AdjFactor"]
    b = pd.concat([pd.read_parquet(f, columns=cols) for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))],
                  ignore_index=True)
    for k in cols[2:]:
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[(b["C"] > 0) & (b["H"] > 0) & (b["L"] > 0)].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    for k in ("H", "L", "C"):
        b[k] = b[k] * later
    by = b["code"]
    pc, ph, pl = (b.groupby("code")[k].shift(1) for k in ("C", "H", "L"))
    tr = pd.concat([b["H"] - b["L"], (b["H"] - pc).abs(), (b["L"] - pc).abs()], axis=1).max(axis=1, skipna=False)
    up, dn = b["H"] - ph, pl - b["L"]
    pdm = pd.Series(np.where((up > dn) & (up > 0), up, 0.0), index=b.index).where(ph.notna())
    mdm = pd.Series(np.where((dn > up) & (dn > 0), dn, 0.0), index=b.index).where(ph.notna())
    atr = tr.groupby(by).transform(lambda x: wilder(x, 14))
    pdi = 100 * pdm.groupby(by).transform(lambda x: wilder(x, 14)) / atr
    mdi = 100 * mdm.groupby(by).transform(lambda x: wilder(x, 14)) / atr
    dx = 100 * (pdi - mdi).abs() / (pdi + mdi).replace(0, np.nan)
    out = pd.DataFrame({"code": b["code"], "d0": b["d0"]})
    out["atr_pct"] = atr / b["C"]
    out["pdi"], out["mdi"] = pdi, mdi
    out["adx"] = dx.groupby(by).transform(lambda x: wilder(x, 14))
    return out


def seen_all(te, pools, seed):
    """test/dt_nscale.seen_ranked と同じ乱数で見えるギャップを作る（業種の上限は掛けない）。"""
    from dt_preopen_sim import BANDS, seen
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    rng = np.random.default_rng(seed)
    e = np.zeros(len(te))
    for b, pool in enumerate(pools):
        if (band == b).any():
            e[band == b] = rng.choice(pool, size=int((band == b).sum()))
    return seen(te, e)


def cap_rank(g, key):
    g = g.sort_values(["d", key, "code"], kind="mergesort")
    g = g[g.groupby(["d", "sector"], dropna=False).cumcount() < 1].copy()
    g["rank"] = g.groupby("d").cumcount() + 1
    g["net"] = (g["y_raw"] - COST) * 1e4
    return g.reset_index(drop=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    ap.add_argument("--i0", type=float, default=10.0)
    ap.add_argument("--cap", type=float, default=1e7)
    ap.add_argument("--slot", default=SNAP_SLOT)
    a = ap.parse_args()
    from dt_preopen_sim import error_pools

    te = pd.read_parquet("test/out/dt_candidates_wide.parquet")
    te = te[te["d"] >= SINCE].copy()
    days = np.sort(te["d"].unique())
    te["d0"] = te["d"].map(pd.Series(days[:-1], index=days[1:]))
    te = te.merge(wilder_features(), on=["code", "d0"], how="left")
    te = te.merge(family_features()[["code", "d0", "srsi"]], on=["code", "d0"], how="left")
    te["D1"] = te["adx"] < 20
    te["D2"] = (te["adx"] >= 25) & (te["mdi"] > te["pdi"])
    te["SRSI"] = te["srsi"] <= 0.2
    pools = error_pools(a.slot, "2026-09-11")
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    C = a.cap
    variants = ["base", "K_ATR", "D1", "D2", "SRSI"]
    acc, share, corr = {}, {}, []
    for seed in range(a.seeds):
        g = seen_all(te, pools, seed)
        atr = g["atr_pct"].fillna(g["vol20"])  # ATR が無い銘柄は vol20 で代える（現行と同じ扱い）
        g["key_atr"] = np.where(atr.notna(), np.round(g["gap"], 4) / np.maximum(atr, VOL_FLOOR), np.inf)
        g0 = cap_rank(g, "key_sort")
        kappa = calib_kappa(g0, a.i0 * 1e-4)
        if seed == 0:
            top = g0[g0["rank"] <= 10]
            share = {k: top[k].fillna(False).mean() for k in ("D1", "D2", "SRSI")}
            ga = cap_rank(g, "key_atr")
            share["K_ATR 上位 10 本の重なり"] = np.mean([len(set(x.code) & set(ga.loc[(ga.d == d) & (ga["rank"] <= 10), "code"]))
                                                   for d, x in top.groupby("d")])
        for v in variants:
            if v == "base":
                x = g0
            elif v == "K_ATR":
                x = cap_rank(g, "key_atr")
            elif v == "D2":
                x = cap_rank(g[~g["D2"].fillna(False)], "key_sort")
            else:
                x = g0.assign(_p=np.where(g0["rank"] <= POOL, np.where(g0[v].fillna(False), 0, 1), 2))
                x = x.sort_values(["d", "_p", "rank"], kind="mergesort")
                x["rank"] = x.groupby("d").cumcount() + 1
            dd = [(d, y) for d, y in x.groupby("d") if not rules.loc[d, "skip"]]
            s = pd.Series({d: pnl_day(y, *alloc_rule(y, 0.002, 20, C * rules.loc[d, "mult"], k=7), kappa) for d, y in dd})
            acc.setdefault(v, []).append(s.reindex(alld).fillna(0.0))
        print(f"seed {seed}", flush=True)

    per = {"IS 2017〜2021": alld <= IS_END, "OOS 2022〜": alld > IS_END, "2023〜": alld >= "2023-01-01"}
    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    print("\n上位 10 本に占める対象: " + "、".join(f"{k} {v:.1%}" if v <= 1 else f"{k} {v:.1f} 本" for k, v in share.items()))
    print(f"\n## 資金 {C / 1e4:.0f} 万・I0 {a.i0:.0f} bp・{a.seeds} シード（円/日。base との差, t）")
    print("| 案 | " + " | ".join(per) + " | OOS 年率・最大DD |")
    print("|---|" + "---|" * (len(per) + 1))
    oos = alld > IS_END
    for v in variants:
        d = mean[v] - mean["base"]
        cells = [f"{mean[v][m].mean():,.0f}" + ("" if v == "base" else f"（{d[m].mean():+,.0f}, {tstat(d[m]):+.2f}）")
                 for m in per.values()]
        ann = np.mean([x[oos].mean() * 245 / C * 100 for x in acc[v]])
        mdd = np.mean([max_dd(x[oos].values) / C * 100 for x in acc[v]])
        print(f"| {v} | " + " | ".join(cells) + f" | {ann:.1f}%・{mdd:.1f}% |")


if __name__ == "__main__":
    main()
