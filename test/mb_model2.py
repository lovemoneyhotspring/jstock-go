#!/usr/bin/env python3
"""第 2 段: 当日の実現ギャップを見てから「寄り後に崩れる／戻す」を予測できるか。

始値は 09:00 に観測できるので、寄り後に建てる取引にとってはリークではない。
事前登録の追記（2026-09-16）に対応。基準は第 1 段と同じ AUC >= 0.60 かつ lift >= 2.0。

追記 2 に合わせ、特徴量にドル円・米 10 年債・SOX を含め、
目的変数は等ウェイトと時価総額加重の両方を測る。
"""

import os

import pandas as pd
from sklearn.metrics import roc_auc_score

from mb_model import FEATURES, IS_END, OOS_START, PANEL, fit_eval, lift

# 当日の寄りで分かる量
OPEN_FEATURES = ["gap_breadth", "cw_gap_breadth", "tpx_gap"]
KINDS = ["intraday", "cw_intraday"]


def main():
    d = pd.read_parquet(PANEL)
    d = d[d.date >= "2016-09-13"].copy()
    d = pd.get_dummies(d, columns=["dow"], prefix="dow", drop_first=True)
    base = [f for f in FEATURES if f in d.columns] + [c for c in d.columns if c.startswith("dow_")]
    feats = base + OPEN_FEATURES

    d = d.dropna(subset=feats).reset_index(drop=True)
    is_df = d[d.date <= IS_END].reset_index(drop=True)
    oos_df = d[d.date >= OOS_START].reset_index(drop=True)
    print(f"特徴量 寄り前 {len(base)} 個 ＋ 当日 {len(OPEN_FEATURES)} 個  "
          f"IS {len(is_df)} 日 / OOS {len(oos_df)} 日\n")

    rows = []
    for kind in KINDS:
        for side in ["down", "up"]:
            col = f"y_{kind}_{side}"
            yi, yo = is_df[col].values, oos_df[col].values
            for name, fs in [("寄り前のみ", base), ("＋当日ギャップ", feats)]:
                a_is, a_oos, l_is, l_oos, m = fit_eval(
                    is_df[fs].astype(float).values, yi,
                    oos_df[fs].astype(float).values, yo)
                rows.append([kind, side, name, int(yi.sum()), int(yo.sum()),
                             round(a_is, 3), round(a_oos, 3),
                             round(l_is, 2), round(l_oos, 2)])
                if name == "＋当日ギャップ":
                    s = pd.Series(m.named_steps["logisticregression"].coef_[0], index=fs)
                    top = s.reindex(s.abs().sort_values(ascending=False).index).head(6)
                    print(f"[{kind}_{side} 係数 上位6]\n{top.round(3).to_string()}\n")

    res = pd.DataFrame(rows, columns=["kind", "side", "特徴量", "n_IS", "n_OOS",
                                      "AUC_IS", "AUC_OOS", "lift_IS", "lift_OOS"])
    print("=== 寄り値を足すと日中は読めるようになるか ===")
    print(res.to_string(index=False))

    print("\n=== 基準（IS・OOS とも AUC>=0.60 かつ lift>=2.0）===")
    for _, r in res[res["特徴量"].str.startswith("＋")].iterrows():
        pas = (r.AUC_IS >= 0.60 and r.AUC_OOS >= 0.60
               and r.lift_IS >= 2.0 and r.lift_OOS >= 2.0)
        print(f"  {r.kind}_{r.side}: {'満たす' if pas else '満たさない'}")

    # ギャップの大きさ別に、その後の日中がどう動いたか（素の姿）
    print("\n=== 当日のギャップ breadth 五分位 → 日中（%）===")
    for label, sub in [("IS", is_df), ("OOS", oos_df)]:
        q = pd.qcut(sub.gap_breadth, 5,
                    labels=["G1(全面安ギャップ)", "G2", "G3", "G4", "G5(全面高ギャップ)"])
        t = pd.DataFrame({
            "n": sub.groupby(q, observed=True).size(),
            "gap": (100 * sub.groupby(q, observed=True).gap_breadth.mean()).round(1),
            "日中_等ｳｪｲﾄ": (100 * sub.groupby(q, observed=True).intraday_breadth.mean()).round(1),
            "日中_時価総額": (100 * sub.groupby(q, observed=True).cw_intraday_breadth.mean()).round(1),
            "TOPIX日中_bp": (((sub.tpx_c / sub.tpx_o - 1.0) * 1e4)
                            .groupby(q, observed=True).mean()).round(1),
        })
        print(f"\n[{label}]")
        print(t.to_string())

    print("\n=== 参考: 当日のギャップ breadth 1 本で日中を予測（順張り方向の AUC）===")
    for label, sub in [("IS", is_df), ("OOS", oos_df)]:
        for kind in KINDS:
            for side in ["down", "up"]:
                y = sub[f"y_{kind}_{side}"].values
                sgn = -1.0 if side == "down" else 1.0
                print(f"  {label} {kind}_{side}: 順張り AUC "
                      f"{roc_auc_score(y, sgn * sub.gap_breadth.values):.3f}")


if __name__ == "__main__":
    main()
