"""ロング: ボリンジャーバンド・RSI などのオシレータと組み合わせたら銘柄選定は良くなるか（5 案）。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_oscillator.py [--top 10]

事前登録（結果を見る前に固定。O3 だけ当日の始値を使い、他は前日の引けまで）:
  母集団・指標は test/dt_prevlow_gap.py と同じ（真の始値・上位 N 本・等金額・流動性コスト込み）。
  期間は test/dt_trend.py と同じ 2017-09 以降。前後は 2017-09〜2022 / 2023〜2026。
  ◎ 優先 = 現行の上位 2N の中で対象を先に取り、残りを現行順で埋める。× 除外 = 対象を外して次点で埋める。
    O1 ◎ RSI(14) ≤ 30（Wilder の平滑）
    O2 ◎ RSI(2) ≤ 10（短期の売られすぎ）
    O3 ◎ ボリンジャー −2σ 割れで寄る: 始値 < 20 日線 − 2 × 20 日の終値の標準偏差
    O4 ◎ ストキャスティクス %K(14) ≤ 20（前日終値の 14 日の高安の中の位置）
    O5 × 買われすぎ: RSI(14) ≥ 70（ギャップダウンでも上げの勢いの中にある銘柄を外す）
  採否: G との日次の差が全期間で t ≥ 2.6（5 本を試すので厳しく）、かつ前後の両方で差 > 0。
  補助（採否に使わない）: 上位 N 本の中での対象の超過（日に畳む）、候補全体の横断回帰（Fama-MacBeth、G と同時）。
過去の値は生値に AdjFactor の後ろ向きの積を掛けて調整する。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_prevlow_gap import CAND, tstat
from dt_trend import START
from dt_wf_target import liq_cost_bp


def rsi(x, n):
    d = x.diff()
    up = d.clip(lower=0).ewm(alpha=1 / n, adjust=False, min_periods=n).mean()
    dn = (-d).clip(lower=0).ewm(alpha=1 / n, adjust=False, min_periods=n).mean()
    return 100 * up / (up + dn)


def osc_features():
    """(code, d0 = 前日) → 前日の引けまでのオシレータ。"""
    cols = ["Code", "Date", "H", "L", "C", "AdjFactor"]
    b = pd.concat([pd.read_parquet(f, columns=cols) for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))],
                  ignore_index=True)
    for k in cols[2:]:
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[b["C"] > 0].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    for k in ("H", "L", "C"):
        b[k] = b[k] * later
    g = b.groupby("code")
    out = pd.DataFrame({"code": b["code"], "d0": b["d0"], "cl": b["C"]})
    out["rsi14"] = g["C"].transform(lambda x: rsi(x, 14))
    out["rsi2"] = g["C"].transform(lambda x: rsi(x, 2))
    out["ma20"] = g["C"].transform(lambda x: x.rolling(20).mean())
    out["sd20"] = g["C"].transform(lambda x: x.rolling(20).std())
    hi = g["H"].transform(lambda x: x.rolling(14).max())
    lo = g["L"].transform(lambda x: x.rolling(14).min())
    out["stoch"] = np.where(hi > lo, 100 * (b["C"] - lo) / (hi - lo), np.nan)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    ap.add_argument("--days", default="all", choices=["all", "gapvol"],
                    help="gapvol は本番で gap_vol で建てる日だけ（米国小幅高・12 月を除く。2026-09-25 追加）。all は元の検証の形")
    a = ap.parse_args()
    n = a.top

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    if a.days == "gapvol":
        from dt_nscale import gapvol_days
        c = c[c["d"].isin(gapvol_days())].copy()
        print(f"本番で gap_vol の日だけ: {c['d'].nunique()} 日")
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(osc_features(), on=["code", "d0"], how="inner")
    c = c[c["d"] >= START].dropna(subset=["rsi14", "rsi2", "sd20", "stoch"])
    c["G"] = c["key_sort"]
    o_adj = c["o"] / c["prev_close"] * c["cl"]  # 始値を調整済みの単位へ（前日終値でつなぐ）
    c["pctb"] = (o_adj - (c["ma20"] - 2 * c["sd20"])) / (4 * c["sd20"]).replace(0, np.nan)  # 始値の %b
    c["O1"] = c["rsi14"] <= 30
    c["O2"] = c["rsi2"] <= 10
    c["O3"] = c["pctb"] < 0
    c["O4"] = c["stoch"] <= 20
    c["O5"] = c["rsi14"] >= 70
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    c = c.sort_values(["d", "G", "code"], kind="mergesort").reset_index(drop=True)
    c["rk"] = c.groupby("d").cumcount()
    flags = {"O1": "◎", "O2": "◎", "O3": "◎", "O4": "◎", "O5": "×"}

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

    feats = ["rsi14", "rsi2", "pctb", "stoch"]

    def fm(x):
        if len(x) < 30:
            return None
        z = lambda s: (s - s.mean()) / (s.std() or 1)
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        xx = x[feats].fillna(x[feats].median())
        return pd.Series({f: np.linalg.lstsq(np.column_stack([np.ones(len(x)), z(x["G"]), z(xx[f])]), y, rcond=None)[0][2]
                          for f in feats})
    r = c.groupby("d")[["G", "y_raw"] + feats].apply(fm).dropna()
    print("\n補助: 候補全体の横断回帰（G と同時、1σ あたり bp。負 = 値が小さい＝売られすぎほど当日上がる）")
    for f in feats:
        print(f"  {f}: " + "、".join(f"{p} {s.mean():+.2f}（t {tstat(s):+.2f}）" for p, s in split(r[f]).items()))


if __name__ == "__main__":
    main()
