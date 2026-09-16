#!/usr/bin/env python3
"""全面安・全面高の予測（2026-09-jp-market-breadth）を IS で学習し OOS で測る。

事前登録（~/obsidian-vault/20-research/2026-09-jp-market-breadth.md）:
  基準 2（本命）= intraday の全面安・全面高が IS・OOS の両方で AUC >= 0.60 かつ lift >= 2.0
  基準 1（参考）= gap は当たっても始値で約定する以上は取れないので採否に使わない

第 2 版（追記 2）でドル円・米 10 年債・SOX と時価総額加重 breadth を足した。
新データの寄与を分けるため、旧特徴量だけのモデルと**同じ行・同じ手順**で並べて比べる。

IS の AUC は当てはめ値ではなくウォークフォワード（TimeSeriesSplit）の out-of-fold で出す。
"""

import os

import numpy as np
import pandas as pd
from sklearn.linear_model import LogisticRegression
from sklearn.metrics import roc_auc_score
from sklearn.model_selection import TimeSeriesSplit
from sklearn.pipeline import make_pipeline
from sklearn.preprocessing import StandardScaler

ROOT = os.path.expanduser("~/jstock-go")
PANEL = os.path.join(ROOT, "test", "out", "mb_panel.parquet")

IS_END = "2021-12-30"
OOS_START = "2022-01-04"
TOP_Q = 0.10          # lift を測る上位の割合

# 第 1 段の特徴量（寄り付き前に確定。p_ は前営業日、spx/vix は前夜の米国）
FEATURES_OLD = [
    "spx_ret1", "spx_ret2", "vix", "vix_chg",
    "p_tpx_ret", "p_tpx_ret5", "p_tpx_vol20", "p_tpx_dev25",
    "p_nk_ret", "p_nk_vi", "p_nk_vi_chg",
    "p_day_breadth", "p5_day_breadth", "p_adr25",
    "p_short_ratio", "frgn_bal_r", "ind_bal_r",
    "is_month_start3", "is_month_end3", "is_sq_week",
]

# 追記 2 で足した外部データ
FEATURES_NEW = [
    "usdjpy_ret1", "usdjpy_ret5", "usdjpy_vol20",
    "ust10", "ust10_chg1", "ust10_chg5",
    "sox_ret1", "sox_ret2",
]

FEATURES = FEATURES_OLD + FEATURES_NEW

KINDS = ["gap", "intraday", "day", "cw_gap", "cw_intraday", "cw_day"]
SIDES = ["down", "up"]


def lift(y, p, q=TOP_Q):
    """予測確率 上位 q の日の発生率 ÷ 全体のベース率。"""
    base = y.mean()
    if base == 0:
        return np.nan
    k = max(1, int(round(len(y) * q)))
    idx = np.argsort(-p)[:k]
    return y[idx].mean() / base


def model():
    return make_pipeline(StandardScaler(), LogisticRegression(C=1.0, max_iter=5000))


def fit_eval(Xi, yi, Xo, yo):
    """IS はウォークフォワードの out-of-fold、OOS は IS 全体で学習した 1 つのモデル。"""
    oof = np.full(len(yi), np.nan)
    for tr, te in TimeSeriesSplit(n_splits=5).split(Xi):
        if yi[tr].sum() < 5:
            continue
        oof[te] = model().fit(Xi[tr], yi[tr]).predict_proba(Xi[te])[:, 1]
    ok = ~np.isnan(oof)
    m = model().fit(Xi, yi)
    po = m.predict_proba(Xo)[:, 1]
    return (roc_auc_score(yi[ok], oof[ok]), roc_auc_score(yo, po),
            lift(yi[ok], oof[ok]), lift(yo, po), m)


def main():
    d = pd.read_parquet(PANEL)
    d = d[d.date >= "2016-09-13"].copy()
    d = pd.get_dummies(d, columns=["dow"], prefix="dow", drop_first=True)
    dows = [c for c in d.columns if c.startswith("dow_")]

    old = [f for f in FEATURES_OLD if f in d.columns] + dows
    new = [f for f in FEATURES_NEW if f in d.columns]
    missing = [f for f in FEATURES_NEW if f not in d.columns]
    both = old + new
    if missing:
        print(f"※ パネルに無い特徴量: {missing}")

    # 両方のモデルを同じ行で比べるため、拡張セットの欠損で揃えて落とす
    d = d.dropna(subset=both).reset_index(drop=True)
    is_df = d[d.date <= IS_END].reset_index(drop=True)
    oos_df = d[d.date >= OOS_START].reset_index(drop=True)
    print(f"旧 {len(old)} 個 ＋ 新 {len(new)} 個  "
          f"IS {len(is_df)} 日 ({is_df.date.min().date()}〜{is_df.date.max().date()})  "
          f"OOS {len(oos_df)} 日 ({oos_df.date.min().date()}〜{oos_df.date.max().date()})\n")

    sets = {"旧のみ": old, "＋新データ": both}
    mats = {k: (is_df[v].astype(float).values, oos_df[v].astype(float).values)
            for k, v in sets.items()}

    rows, coefs = [], {}
    for kind in KINDS:
        for side in SIDES:
            col = f"y_{kind}_{side}"
            yi, yo = is_df[col].values, oos_df[col].values
            if yi.sum() < 10 or yo.sum() < 10:
                rows.append([kind, side, int(yi.sum()), int(yo.sum())] + [np.nan] * 8)
                continue
            r = [kind, side, int(yi.sum()), int(yo.sum())]
            for name in sets:
                Xi, Xo = mats[name]
                a_is, a_oos, l_is, l_oos, m = fit_eval(Xi, yi, Xo, yo)
                r += [round(a_is, 3), round(a_oos, 3), round(l_is, 2), round(l_oos, 2)]
                if name == "＋新データ":
                    coefs[f"{kind}_{side}"] = pd.Series(
                        m.named_steps["logisticregression"].coef_[0], index=sets[name])
            rows.append(r)

    res = pd.DataFrame(rows, columns=[
        "kind", "side", "n_IS", "n_OOS",
        "AUCis_旧", "AUCoos_旧", "Lis_旧", "Loos_旧",
        "AUCis_新", "AUCoos_新", "Lis_新", "Loos_新"])
    print("=== 旧特徴量だけ vs ＋ドル円・米10年債・SOX ===")
    print(res.to_string(index=False))

    print("\n=== 事前登録の基準（日中が IS・OOS とも AUC>=0.60 かつ lift>=2.0）===")
    for _, r in res[res.kind.isin(["intraday", "cw_intraday"])].iterrows():
        for tag, a_is, a_oos, l_is, l_oos in [
                ("旧のみ", r["AUCis_旧"], r["AUCoos_旧"], r["Lis_旧"], r["Loos_旧"]),
                ("＋新", r["AUCis_新"], r["AUCoos_新"], r["Lis_新"], r["Loos_新"])]:
            pas = a_is >= 0.60 and a_oos >= 0.60 and l_is >= 2.0 and l_oos >= 2.0
            print(f"  {r.kind}_{r.side} [{tag}]: {'満たす' if pas else '満たさない'}"
                  f" (AUC {a_is}/{a_oos}, lift {l_is}/{l_oos})")

    print("\n=== 新データの係数（＋新データのモデル、日中のみ）===")
    for k in ["intraday_down", "intraday_up", "cw_intraday_down", "cw_intraday_up"]:
        if k in coefs:
            print(f"\n[{k}]")
            print(coefs[k][new].round(3).to_string())

    print("\n=== 参考: 単一特徴量の OOS AUC（符号つき。down は逆符号で見る）===")
    singles = ["spx_ret1"] + new
    hdr = "  " + "".join(f"{s:>14}" for s in ["特徴量"] + [f"{k}_{s}" for k in
                                              ["gap", "intraday"] for s in SIDES])
    print(hdr)
    for f in singles:
        line = f"  {f:>12}"
        for kind in ["gap", "intraday"]:
            for side in SIDES:
                yo = oos_df[f"y_{kind}_{side}"].values
                sgn = -1.0 if side == "down" else 1.0
                line += f"{roc_auc_score(yo, sgn * oos_df[f].values):>14.3f}"
        print(line)


if __name__ == "__main__":
    main()
