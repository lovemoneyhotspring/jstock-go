"""ロング: 前日・前々日の値動き（「3 日」の節目）で銘柄選択を良くできるか。

  test/.venv/bin/python test/dt_candidates.py          # test/out/dt_candidates.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_three_day.py [--top 10]

仮説（ユーザ、2026-09-25）: 人は 3 日を節目に見るので、前日・前々日の値動きは当日の寄→引に効くはず。

事前登録（結果を見る前に固定。日付 d は当日、d-1 は前日）:
  母集団・期間・指標は test/dt_prevlow_gap.py と同じ（真の始値・上位 N 本・等金額・流動性コスト込み）。
  案（仮説の向きで動かす。◎ は優先 = 現行の上位 2N の中で対象を先に取り、残りを現行順で埋める、
     × は除外 = 対象を外して次点で埋める）
    A ◎ 3 日目の投げ: 前日・前々日とも終値が下げ（2 日続落）、そこへ当日のギャップダウン
    B ランク替え: 3 日＋寄りの累積下落 (始値 / d-4 終値 − 1) / (vol20·√4) で並べる（B）、G と順位平均（BG）
    C × 3 日安値を割らない: 始値 ≥ min(d-1, d-2, d-3 の安値)
    D ◎ 三空（ゆるい定義）: d-2・d-1・当日の 3 回とも前日終値より下で寄った
  採否: G との日次の差が全期間で t ≥ 2.5（6 本を試すので 2 より厳しく）、かつ 2016〜2022・2023〜2026 の両方で差 > 0。
  補助（採否には使わない）: 上位 N 本の中での対象の超過（日に畳む）、候補全体の横断回帰で
    前日・前々日の各量が G を知った上で追加の情報を持つか（Fama-MacBeth）。
過去の値は調整済み（Adj*）の比で作り、当日の始値（生値）とは前日終値を介してつなぐ。
"""

import argparse
import glob

import numpy as np
import pandas as pd

from dt_prevlow_gap import CAND, VOL_FLOOR, tstat
from dt_wf_target import liq_cost_bp


def lag_features():
    """(code, d0 = 前日) → 前日までの値動き。"""
    cols = ["Code", "Date", "O", "L", "C", "AdjFactor"]
    b = pd.concat([pd.read_parquet(f, columns=cols) for f in sorted(glob.glob("data/jquants/equities_bars_daily/*.parquet"))],
                  ignore_index=True)
    for k in cols[2:]:
        b[k] = pd.to_numeric(b[k], errors="coerce")
    b = b[b["C"] > 0].rename(columns={"Code": "code"})
    b["d0"] = pd.to_datetime(b["Date"])
    b = b.sort_values(["code", "d0"]).reset_index(drop=True)
    # AdjFactor は分割の効力日に付く（1:2 なら 0.5）。それより前の値に後ろの係数の積を掛けて今の単位へそろえる
    f = b["AdjFactor"].fillna(1.0).where(lambda x: x > 0, 1.0)
    later = f.groupby(b["code"]).transform(lambda x: x[::-1].cumprod()[::-1].shift(-1, fill_value=1.0))
    for k in ("O", "L", "C"):
        b["Adj" + k] = b[k] * later
    g = b.groupby("code")
    C, O, L = b["AdjC"], b["AdjO"], b["AdjL"]
    C1, C2, C3 = g["AdjC"].shift(1), g["AdjC"].shift(2), g["AdjC"].shift(3)
    out = pd.DataFrame({"code": b["code"], "d0": b["d0"]})
    out["r_d1"] = C / C1 - 1                    # 前日の終値の騰落
    out["r_d2"] = C1 / C2 - 1                   # 前々日の終値の騰落
    out["id_d1"] = C / O - 1                    # 前日の寄→引
    out["id_d2"] = g["AdjC"].shift(1) / g["AdjO"].shift(1) - 1
    out["gd_d1"] = O < C1                       # 前日はギャップダウンで寄った
    out["gd_d2"] = g["AdjO"].shift(1) < C2      # 前々日も
    out["c_over_c3"] = C / C3                   # 前日終値 / d-4 終値
    low3 = pd.concat([L, g["AdjL"].shift(1), g["AdjL"].shift(2)], axis=1).min(axis=1, skipna=False)
    out["low3_c"] = low3 / C                    # 3 日安値 / 前日終値
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--top", type=int, default=10)
    ap.add_argument("--days", default="all", choices=["all", "gapvol", "uslow", "december"],
                    help="gapvol は本番で gap_vol で建てる日だけ（米国小幅高・12 月を除く）、uslow は米国小幅高の日（12 月を除く）、"
                         "december は 12 月だけ（2026-09-25 追加）。all は元の検証の形")
    a = ap.parse_args()
    n = a.top

    c = pd.read_parquet(CAND)
    c = c[np.isfinite(c["key_sort"])].copy()
    if a.days != "all":
        from dt_nscale import day_kind
        k = day_kind()
        c = c[c["d"].isin(k.index[k.values == a.days])].copy()
        print(f"日の種類 {a.days} だけ: {c['d'].nunique()} 日")
    days = np.sort(c["d"].unique())
    c["d0"] = c["d"].map(pd.Series(days[:-1], index=days[1:]))
    c = c.merge(lag_features(), on=["code", "d0"], how="inner").dropna(subset=["r_d1", "r_d2", "c_over_c3", "low3_c"])
    v = np.maximum(c["vol20"], VOL_FLOOR)
    c["G"] = c["key_sort"]
    c["A"] = (c["r_d1"] < 0) & (c["r_d2"] < 0)
    c["cum4"] = (1 + c["gap"]) * c["c_over_c3"] - 1
    c["B"] = c["cum4"] / (v * 2)
    c["BG"] = c.groupby("d")["G"].rank() + c.groupby("d")["B"].rank()
    c["C"] = c["o"] >= c["prev_close"] * c["low3_c"]
    c["D"] = c["gd_d1"] & c["gd_d2"]
    c["ret"] = (c["y_raw"] - liq_cost_bp(c["turnover_med"].values) / 1e4) * 1e4
    c = c.sort_values(["d", "G", "code"], kind="mergesort").reset_index(drop=True)
    c["rk"] = c.groupby("d").cumcount()
    print(f"{len(c):,} 行 / {c['d'].nunique():,} 日")

    def top_by(key):
        return c.sort_values(["d", key, "code"], kind="mergesort").groupby("d").head(n).groupby("d")["ret"].mean()

    def prefer(flag):
        pool = c[c["rk"] < 2 * n].assign(p=lambda x: (~x[flag]).astype(int))
        return pool.sort_values(["d", "p", "rk"]).groupby("d").head(n).groupby("d")["ret"].mean()

    def exclude(flag):
        return c[~c[flag]].groupby("d").head(n).groupby("d")["ret"].mean()

    base = c[c["rk"] < n]
    daily = pd.DataFrame({"G": base.groupby("d")["ret"].mean(), "A◎": prefer("A"), "B": top_by("B"),
                          "BG": top_by("BG"), "C×": exclude("C"), "D◎": prefer("D")})
    yr = daily.index.year
    periods = {"全期間": np.ones(len(daily), bool), "2016〜2022": yr <= 2022, "2023〜2026": yr >= 2023}
    print("上位 N 本に占める対象: " + "、".join(f"{f} {base[f].mean():.1%}" for f in ("A", "C", "D"))
          + f"（上位 2N では A {c[c.rk < 2 * n]['A'].mean():.1%}・D {c[c.rk < 2 * n]['D'].mean():.1%}）")
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

    base = base.assign(ex=base["ret"] - base.groupby("d")["ret"].transform("mean"))
    print(f"\n補助: 現行の上位 {n} 本の中での対象の超過（bp、日に畳む）")
    for f in ("A", "C", "D"):
        s = base[base[f]].groupby("d")["ex"].mean()
        parts = {"全期間": s, "2016〜2022": s[s.index.year <= 2022], "2023〜2026": s[s.index.year >= 2023]}
        print(f"  {f}: " + "、".join(f"{p} {x.mean():+.1f}（t {tstat(x):+.2f}, {len(x)} 日）" for p, x in parts.items()))

    print("\n補助: 候補全体の横断回帰（G と同時、1σ あたり bp、t）")
    feats = ["r_d1", "r_d2", "id_d1", "id_d2", "cum4"]

    def fm(x):
        if len(x) < 30:
            return None
        z = lambda s: (s - s.mean()) / (s.std() or 1)
        y = x["y_raw"].clip(x["y_raw"].quantile(0.01), x["y_raw"].quantile(0.99)).values * 1e4
        res = {}
        for f in feats:
            X = np.column_stack([np.ones(len(x)), z(x["G"]), z(x[f])])
            res[f] = np.linalg.lstsq(X, y, rcond=None)[0][2]
        return pd.Series(res)
    x = c[c["G"].notna()]
    f = x.groupby("d")[["G", "y_raw"] + feats].apply(fm).dropna()
    for k in feats:
        s = f[k]
        print(f"  {k}: 全期間 {s.mean():+.2f}（t {tstat(s):+.2f}）、2016〜2022 {s[s.index.year <= 2022].mean():+.2f}"
              f"（t {tstat(s[s.index.year <= 2022]):+.2f}）、2023〜2026 {s[s.index.year >= 2023].mean():+.2f}"
              f"（t {tstat(s[s.index.year >= 2023]):+.2f}）")


if __name__ == "__main__":
    main()
