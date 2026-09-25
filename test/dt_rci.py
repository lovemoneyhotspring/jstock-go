"""ロング: RCI（順位相関指数）の売られすぎを gap_vol の上位の中で先に取ったら、銘柄選定は良くなるか（一次の横断）。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md の追記（2026-09-26、ユーザの問い）。仲間の RSI(2)・ストキャス RSI・
2 日続落は 9/25 に不採用、ボリンジャー −2σ（O3）は一次で差なし。同じ候補表でのオシレータの探索の 7 本目なので基準を厳しく置く。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_rci.py

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  RCI(9): 前日の引けまでの 9 日の調整済み終値について、日付の順（古い = 1）と値段の順（安い = 1）のスピアマンの順位相関 × 100。
          +100 = 9 日きれいに上げ続けた、−100 = きれいに下げ続けた。同値は出現順（まれ）。
  案 R1 ◎ RCI(9) ≤ −80 の銘柄を、現行（gap_vol）の上位 20 本の中で先に取り、残りを現行順で埋めて上位 10 本（等金額）。
  母集団・指標は test/dt_oscillator.py と同じ（真の始値・流動性コスト込み・日に畳む）。対象の日は本番で gap_vol を使う日だけ
  （dt_nscale.day_kind の gapvol: 米国小幅高・12 月を除く）。期間 2017-09〜、前後は 2017-09〜2022 / 2023〜2026。
  採否: G との日次の差が全期間で t ≥ 2.6 かつ差 > 0、かつ前後の両方で差 > 0。満たせば本番の形の模擬（誤差・規則 R・単元）へ進む。
  補助（採否に使わない）: 上位 10 本に占める対象、上位 10 本の中での対象の超過、候補全体の横断回帰（G と同時、1σ あたり）。
"""

import glob

import numpy as np
import pandas as pd
from numpy.lib.stride_tricks import sliding_window_view

from dt_nscale import day_kind
from dt_prevlow_gap import CAND, tstat
from dt_trend import START
from dt_wf_target import liq_cost_bp

N_RCI, TH, TOP = 9, -80, 10


def rci_frame():
    """(code, d0 = 前日) → RCI(9)。過去の値は生値に AdjFactor の後ろ向きの積を掛けて調整する（dt_oscillator と同じ）。"""
    cols = ["Code", "Date", "C", "AdjFactor"]
    b = pd.concat([pd.read_parquet(f, columns=cols) for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))],
                  ignore_index=True)
    for k in ("C", "AdjFactor"):
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[b["C"] > 0].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    b["C"] = b["C"] * later
    date_rank = np.arange(1, N_RCI + 1, dtype=float)
    denom = N_RCI * (N_RCI ** 2 - 1)
    out = np.full(len(b), np.nan)
    for _, idx in b.groupby("code").indices.items():
        if len(idx) < N_RCI:
            continue
        w = sliding_window_view(b["C"].values[idx], N_RCI)
        pr = w.argsort(axis=1).argsort(axis=1) + 1.0
        out[idx[N_RCI - 1:]] = (1 - 6 * ((date_rank - pr) ** 2).sum(axis=1) / denom) * 100
    return pd.DataFrame({"code": b["code"], "d0": b["d0"], "rci9": out})


def main():
    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    k = day_kind()
    c = c[c["d"].isin(k.index[k.values == "gapvol"])].copy()
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(rci_frame(), on=["code", "d0"], how="inner")
    c = c[c["d"] >= START].dropna(subset=["rci9"])
    c["G"] = c["key_sort"]
    c["R1"] = c["rci9"] <= TH
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    c = c.sort_values(["d", "G", "code"], kind="mergesort").reset_index(drop=True)
    c["rk"] = c.groupby("d").cumcount()

    base = c[c["rk"] < TOP]
    pool = c[c["rk"] < 2 * TOP].assign(p=lambda x: (~x["R1"]).astype(int))
    r1 = pool.sort_values(["d", "p", "rk"]).groupby("d").head(TOP).groupby("d")["ret"].mean()
    daily = pd.DataFrame({"G": base.groupby("d")["ret"].mean(), "R1": r1})
    diff = daily["R1"] - daily["G"]
    yr = daily.index.year
    periods = {"全期間": np.ones(len(daily), bool), "2017-09〜2022": yr <= 2022, "2023〜2026": yr >= 2023}
    print(f"gap_vol の日だけ: {c['d'].nunique():,} 日、{len(c):,} 行")
    print(f"上位 {TOP} 本に占める RCI(9) ≤ {TH}: {base['R1'].mean():.1%}（上位 {2 * TOP}: {c[c.rk < 2 * TOP]['R1'].mean():.1%}）")
    print(f"\n上位 {TOP} 本（bp/日、G との差, t）")
    for p, m in periods.items():
        print(f"  {p}: G {daily['G'][m].mean():+.2f}、R1 {daily['R1'][m].mean():+.2f}（{diff[m].mean():+.2f}, t {tstat(diff[m]):+.2f}）")
    ok = diff.mean() > 0 and tstat(diff) >= 2.6 and all(diff[m].mean() > 0 for m in list(periods.values())[1:])
    print("判定: " + ("通る（本番の形の模擬へ）" if ok else "不採用"))

    base = base.assign(ex=base["ret"] - base.groupby("d")["ret"].transform("mean"))
    s = base[base["R1"]].groupby("d")["ex"].mean()
    print("\n補助: 上位 10 本の中での対象の超過（bp、日に畳む）: " + "、".join(
        f"{p} {x.mean():+.1f}（t {tstat(x):+.2f}, {len(x)} 日）"
        for p, x in {"全期間": s, "〜2022": s[s.index.year <= 2022], "2023〜": s[s.index.year >= 2023]}.items()))

    def fm(x):
        if len(x) < 30:
            return np.nan
        z = lambda v: (v - v.mean()) / (v.std() or 1)
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        return np.linalg.lstsq(np.column_stack([np.ones(len(x)), z(x["G"]), z(x["rci9"])]), y, rcond=None)[0][2]
    r = c.groupby("d")[["G", "y_raw", "rci9"]].apply(fm).dropna()
    print("補助: 候補全体の横断回帰（G と同時、RCI 1σ あたり bp。負 = 下げ続けた銘柄ほど当日上がる）: " + "、".join(
        f"{p} {x.mean():+.2f}（t {tstat(x):+.2f}）"
        for p, x in {"全期間": r, "〜2022": r[r.index.year <= 2022], "2023〜": r[r.index.year >= 2023]}.items()))


if __name__ == "__main__":
    main()
