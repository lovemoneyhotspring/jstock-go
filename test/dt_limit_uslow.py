"""米国小幅高の日（今は両脚とも休み）だけ、LightGBM で並べて寄指で出したら: 休む（= 0）と比べる。

根拠: vault 20-research/2026-09-jp-daytrade-limit-on-open.md の「事前登録 3」（2026-09-21）

  test/.venv/bin/python test/dt_limit_uslow.py [--seeds 20] [--side-seeds 10] [--slot 0859] [--err-since 2026-09-11]
  test/.venv/bin/python test/dt_limit_uslow.py --report-only

判定は 1 本: U = LightGBM 順・上位 3 本・指値 = 前日終値 −0.5%（始値のギャップ < −0.5% だけ約定、残りの枠は現金）。
小幅高の日の定義は本番と同じ（S&P500 の前日比 0〜+1%・VIX ≤ 24）。それ以外の日は B のままなので、差はこの日の損益だけ。
モデルは test/dt_rank_compare.py と同じ拡大窓の 7 本、誤差・選定の規則は test/dt_limit_on_open.py と同じ。
gap_vol の側・ほかの指値・寄成は探索として並べるだけ。
"""

import argparse

import duckdb
import numpy as np
import pandas as pd

from dt_lgbm_train import ranked, raw_features
from dt_limit_on_open import CAPITAL, N_BASE, select
from dt_preopen_sim import BANDS, ERR_SQL, seen
from dt_rank_compare import CAND, LAST_DAY, SEEN_FROM, US, draw, fit, tstat
from dt_wf_target import EMBARGO, FOLD_STARTS

LIMITS = [np.inf, 0.0, -0.005, -0.01, -0.015, -0.02]  # 指値（ギャップ）。inf は寄成
PRIMARY = ("lgbm", -0.005)
OUT = "test/out/dt_limit_uslow_picks.parquet"


def label(c):
    return "寄成" if np.isinf(c) else f"{c * 100:g}%"


def us_low_days():
    us = pd.read_parquet(US)[["date", "spx_ret1", "vix"]].rename(columns={"date": "d"})
    low = (us["spx_ret1"] >= 0) & (us["spx_ret1"] < 0.01) & (us["vix"].isna() | (us["vix"] <= 24))
    return set(us.loc[low, "d"])


def report(picks, n_all):
    days = pd.DatetimeIndex(sorted(picks["d"].unique()))
    unseen = days < SEEN_FROM
    print(f"\n小幅高の日 {len(days)} 日 / 全 {n_all} 日（12 月を除く）。bp は小幅高の日あたり、（）内は全日に均した値。比べる相手は休む = 0")
    for form in ("block", "block_x0.8"):
        pf = picks[picks["form"] == form]
        seeds = sorted(pf["seed"].unique())
        print(f"\n## 誤差の形 {form}（シード {len(seeds)} 本）")
        print("| 並べ方 | 指値 | bp/日（全日換算） | t | 未見 / 既見 | 正のシード | 約定率 | 約定した側 / しなかった側（bp） | 始値が 0% 以上の割合 |")
        print("|---|---|---|---|---|---|---|---|---|")
        for ranker in ("lgbm", "gap_vol"):
            pr = pf[pf["ranker"] == ranker]
            for lim in LIMITS:
                one = lambda s: (pr[pr["seed"] == s].assign(x=lambda q: q["w"] * q["net"] * (q["gap_true"] < lim))
                                 .groupby("d")["x"].sum().reindex(days).fillna(0.0))
                per = [one(s) for s in seeds]
                x = sum(per) / len(seeds)
                fill = pr["gap_true"] < lim
                mark = " **（判定）**" if (ranker, lim) == PRIMARY else ""
                print(f"| {ranker} | {label(lim)}{mark} | {x.mean() * 1e4:+.2f}（{x.sum() / n_all * 1e4:+.2f}） | {tstat(x):.2f} |"
                      f" {x[unseen].mean() * 1e4:+.2f} / {x[~unseen].mean() * 1e4:+.2f} | {sum(p.mean() > 0 for p in per)}/{len(seeds)} |"
                      f" {fill.mean() * 100:.0f}% | {pr.loc[fill, 'net'].mean() * 1e4:+.1f} / {pr.loc[~fill, 'net'].mean() * 1e4:+.1f} |"
                      f" {(pr['gap_true'] >= 0).mean() * 100:.0f}% |")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--side-seeds", type=int, default=10)
    ap.add_argument("--slot", default="0859")
    ap.add_argument("--err-since", default="2026-09-11")
    ap.add_argument("--report-only", action="store_true")
    a = ap.parse_args()

    df = pd.read_parquet(CAND, columns=["d"]) if a.report_only else pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY]
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    in_test = (df["d"] >= bounds[0]) & (df["d"].dt.month != 12)
    n_all = df.loc[in_test, "d"].nunique()

    if not a.report_only:
        err = duckdb.sql(ERR_SQL, params=[a.slot, a.err_since, a.err_since]).df()
        print(f"誤差の実測: slot {a.slot}、{err['d'].nunique()} 日、{len(err):,} 行", flush=True)
        err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
        df = df.copy()
        df["price"] = df["o"]
        df["sector"] = df["sector"].fillna("")
        days = np.array(sorted(df["d"].unique()))
        models = []
        for k in range(len(FOLD_STARTS)):
            train_days = days[days < bounds[k]][:-EMBARGO]
            models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)]))
            print(f"{FOLD_STARTS[k]}: 木 {models[-1].n_estimators} 本", flush=True)

        te = df[in_test].copy()
        band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
        day_idx = te["d"].rank(method="dense").astype(int).values - 1
        low = te["d"].isin(us_low_days()).values  # 誤差は全日で引いてから小幅高の日だけ残す（乱数の並びをほかの模擬と揃える）
        te_low = te[low]
        fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te_low["d"].values, side="right"), index=te_low.index)
        res = []
        for form, scale, n in (("block", 1.0, a.seeds), ("block_x0.8", 0.8, a.side_seeds)):
            for s in range(n):
                e = draw(np.random.default_rng(s), err, band, day_idx, "block") * scale
                g = seen(te_low, e[low])
                X = ranked(raw_features(g), g["d"]).values
                gf = fold_of.loc[g.index].values
                score = np.empty(len(g))
                for k, model in enumerate(models):
                    score[gf == k] = model.predict(X[gf == k])
                g = g.assign(score=score)
                for ranker, order in (("lgbm", g.sort_values(["d", "score", "rule_rank"], ascending=[True, False, True], kind="mergesort")),
                                      ("gap_vol", g.sort_values(["d", "rule_rank"], kind="mergesort"))):
                    p = select(order, N_BASE, CAPITAL / N_BASE)
                    res.append(p[["d", "code", "gap", "gap_true", "w", "net"]].assign(ranker=ranker, form=form, seed=s))
                print(f"{form} seed {s}", flush=True)
        pd.concat(res, ignore_index=True).to_parquet(OUT, index=False)
    report(pd.read_parquet(OUT), n_all)


if __name__ == "__main__":
    main()
