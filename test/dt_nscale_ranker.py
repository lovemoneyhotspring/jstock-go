"""daytrade ロング: 規則 R÷7 の配分で、並べ方を gap_vol と LightGBM で比べる。

根拠・事前登録: vault 20-research/2026-09-jp-daytrade-nscale.md「事前登録（R÷7 での並べ方）」

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_nscale_ranker.py [--seeds 5]

LightGBM の学習・誤差の引き方は test/dt_rank_compare.py と同じ（walk-forward 7 本、slot 0859 の誤差を帯ごとに iid）。
配分・滑り・日の規則は test/dt_nscale.py（alloc_rule・pnl_day・day_rules）と同じ。
"""

import argparse

import duckdb
import numpy as np
import pandas as pd

from dt_lgbm_train import ranked, raw_features
from dt_nscale import (COST, FEE, IS_END, alloc_fixed_iv, alloc_rule, calib_kappa, day_rules, max_dd, pnl_day,
                       tstat)
from dt_preopen_sim import BANDS, ERR_SQL, seen
from dt_rank_compare import CAND, EMBARGO, LAST_DAY, SEEN_FROM, draw, fit
from dt_wf_target import FOLD_STARTS, liq_cost_bp

CAPS = [7e6, 1.5e7, 3e7]


def ranked_by(g, score):
    """score の高い順に並べて業種の上限を掛け、順位を付け直す。"""
    g = g.assign(score=score).sort_values(["d", "score", "code"], ascending=[True, False, True], kind="mergesort")
    g = g[~g.duplicated(["d", "sector"])].copy()
    g["rank"] = g.groupby("d").cumcount() + 1
    g["net"] = (g["y_raw"] - COST) * 1e4
    return g.reset_index(drop=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=5)
    ap.add_argument("--i0", type=float, nargs="+", default=[5.0, 3.0])
    ap.add_argument("--liq-cost", action="store_true",
                    help="流動性別コスト（dt_wf_target.liq_cost_bp）の 5.7 bp を超える分を上乗せする（感度）")
    a = ap.parse_args()
    err = duckdb.sql(ERR_SQL, params=["0859", "2026-09-11", "2026-09-11"]).df()
    err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    models = []
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)]))
    print(f"学習 {len(models)} 本（木 {[m.n_estimators for m in models]}）", flush=True)
    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"),
                        index=te.index)
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    day_idx = te["d"].rank(method="dense").astype(int).values - 1
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    acc = {}
    for s in range(a.seeds):
        e = draw(np.random.default_rng(s), err, band, day_idx, "iid")
        g = seen(te, e)
        X = ranked(raw_features(g), g["d"]).values
        gf = fold_of.loc[g.index].values
        score = np.empty(len(g))
        for k, model in enumerate(models):
            score[gf == k] = model.predict(X[gf == k])
        if a.liq_cost:
            # 学習の後、評価だけに掛ける。pnl_day は y_raw − FEE − 滑りなので、流動性別コストの 5.7 bp を超える分
            # （0 / 5 / 10 / 20 bp）を y_raw から引く
            g = g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
        # kappa は両者で同じ値（gap_vol の上位 3・100 万で I0 に合わせる）
        kappas = {i0: calib_kappa(ranked_by(g, -g["key_sort"].values), i0 * 1e-4) for i0 in a.i0}
        for ranker, sc in (("gap_vol", -g["key_sort"].values), ("lgbm", score)):
            r = ranked_by(g, sc)
            dd = [(d, x) for d, x in r.groupby("d") if not rules.loc[d, "skip"]]
            for i0 in a.i0:
                kappa = kappas[i0]
                for C in CAPS:
                    for form, fn in (("R÷7", lambda x, c: alloc_rule(x, 0.002, 20, c, k=7)),
                                     ("N=3", lambda x, c: alloc_fixed_iv(x, 3, c))):
                        v = pd.Series({d: pnl_day(x, *fn(x, C * rules.loc[d, "mult"]), kappa) for d, x in dd})
                        acc.setdefault((i0, C, form, ranker), []).append(v.reindex(alld).fillna(0.0))
        print(f"seed {s}", flush=True)
    print(f"\n期間 {alld[0]:%Y-%m-%d}〜{alld[-1]:%Y-%m-%d}（12 月を除く {len(alld)} 日）、未見 〜{SEEN_FROM:%Y-%m-%d} / 既見 以降")
    for i0 in a.i0:
        for C in CAPS:
            for form in ("R÷7", "N=3"):
                gv = pd.concat(acc[(i0, C, form, "gap_vol")], axis=1)
                lg = pd.concat(acc[(i0, C, form, "lgbm")], axis=1)
                row = []
                for lab, sl in (("未見", alld < SEEN_FROM), ("既見", alld >= SEEN_FROM), ("全", alld == alld)):
                    ann = lambda m: m[sl].mean().mean() * 245 / C * 100
                    diff = (lg.mean(axis=1) - gv.mean(axis=1))[sl]
                    ddg = np.mean([max_dd(gv[c][sl].values) / C * 100 for c in gv])
                    ddl = np.mean([max_dd(lg[c][sl].values) / C * 100 for c in lg])
                    row.append(f"{lab} gap_vol {ann(gv):5.1f}% lgbm {ann(lg):5.1f}% 差 {diff.mean()*245/1e4:+6.1f}万/年 t {tstat(diff):5.2f}"
                               f" DD {ddg:4.1f}/{ddl:4.1f}%")
                print(f"  I0 {i0:.0f} 資金 {C/1e4:4.0f} 万 {form}: " + " | ".join(row))


if __name__ == "__main__":
    main()
