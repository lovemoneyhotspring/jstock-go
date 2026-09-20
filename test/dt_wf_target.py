"""daytrade ロングの並べ替えモデルの目的変数を替えて、学習期間の外で比べる（walk-forward）。

根拠・事前登録: vault 20-research/2026-09-jp-daytrade-ml-target-magnitude.md

  test/.venv/bin/python test/dt_candidates.py
  test/.venv/bin/python test/dt_wf_target.py [--variants base,t1,t1p,t2] [--seeds 5] [--tag main]

出力: test/out/dt_wf_target_<tag>_daily.parquet（日 × 形 × シードの損益）、_picks.parquet（建てた銘柄）
集計は test/dt_wf_target_report.py。
学習の設定・特徴量は本番（test/dt_lgbm_train.py）をそのまま使う。
"""

import argparse
import time

import numpy as np
import pandas as pd
from lightgbm import LGBMRanker, LGBMRegressor, early_stopping, log_evaluation

from dt_lgbm_train import INNER_VALID_DAYS, KW, VOL_FLOOR, ranked, raw_features

CAND = "test/out/dt_candidates.parquet"
FOLD_STARTS = [f"{y}-09-17" for y in range(2019, 2026)]
LAST_DAY = "2026-09-16"
EMBARGO = 5
N = 3
BUDGET = 1_666_666  # 本番（config/daytrade_margin）のロングの 1 注文: max_capital 500 万 ÷ N 3
LABEL_MAX = 20


def liq_cost_bp(turnover):
    """流動性別コスト（往復、bp）。10 億以上は一律と同じ 5.7。"""
    return np.select([turnover >= 1e9, turnover >= 5e8, turnover >= 3e8], [5.7, 10.7, 15.7], 25.7)


def targets(df, clip):
    ex = df["y_raw"] - df.groupby("d")["y_raw"].transform("mean")
    exc = ex.clip(-clip, clip)
    return exc, ((exc + clip) / (2 * clip) * LABEL_MAX).round().astype(int)


def fit_predict(variant, seed, Xtr, ytr, dtr, Xte):
    """末尾 250 日で木の本数を決め、学習期間の全体で学習し直して検証期間を予測する。"""
    days = np.array(sorted(dtr.unique()))
    core = (dtr < days[len(days) - INNER_VALID_DAYS]).values
    kw = dict(KW, random_state=seed)
    cb = [early_stopping(50, verbose=False), log_evaluation(0)]
    if variant == "t2":
        kw.update(objective="lambdarank", label_gain=list(range(LABEL_MAX + 1)))
        grp = lambda mask: dtr[mask].groupby(dtr[mask], sort=False).size().values  # noqa: E731
        m = LGBMRanker(n_estimators=2000, **kw)
        m.fit(Xtr[core], ytr[core], group=grp(core), eval_set=[(Xtr[~core], ytr[~core])], eval_group=[grp(~core)],
              eval_at=[N], callbacks=cb)
        best = m.best_iteration_ or 200
        final = LGBMRanker(n_estimators=best, **kw)
        final.fit(Xtr, ytr, group=dtr.groupby(dtr, sort=False).size().values)
    else:
        m = LGBMRegressor(n_estimators=2000, **kw)
        m.fit(Xtr[core], ytr[core], eval_set=[(Xtr[~core], ytr[~core])], eval_metric="l2", callbacks=cb)
        best = m.best_iteration_ or 200
        final = LGBMRegressor(n_estimators=best, **kw)
        final.fit(Xtr, ytr)
    return final.predict(Xte), best


def pick(day):
    """予測の高い順に N 本。1 単元が予算を超える銘柄は飛ばし、業種は 1 銘柄まで（業種が空なら数えない）。"""
    out, seen = [], set()
    for r in day.itertuples():
        if r.price * 100 > BUDGET:  # price は選ぶ時点で見える値段（walk-forward では始値、気配の模擬では気配）
            continue
        if r.sector and r.sector in seen:
            continue
        if r.sector:
            seen.add(r.sector)
        out.append(r.Index)
        if len(out) == N:
            break
    return out


def evaluate(te, score, variant, seed):
    t = te.assign(score=score).sort_values(["d", "score", "rule_rank"], ascending=[True, False, True], kind="mergesort")
    idx = [i for _, g in t.groupby("d", sort=False) for i in pick(g.head(60))]
    p = t.loc[idx].copy()
    inv = 1.0 / np.maximum(p["vol20"].fillna(VOL_FLOOR), VOL_FLOOR)
    p["w"] = inv / inv.groupby(p["d"]).transform("sum")
    p["variant"], p["seed"] = variant, seed
    return p[["d", "code", "variant", "seed", "w", "gap", "y_raw", "turnover_med", "rule_rank"]]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--variants", default="base,t1,t1p,t2")
    ap.add_argument("--seeds", type=int, default=5)
    ap.add_argument("--tag", default="main")
    a = ap.parse_args()
    variants = a.variants.split(",")

    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    df = df.sort_values(["d", "key_sort", "code"], kind="mergesort").reset_index(drop=True)
    df["rule_rank"] = df.groupby("d").cumcount() + 1
    X = ranked(raw_features(df), df["d"]).values
    cost = liq_cost_bp(df["turnover_med"].values) / 1e4

    y = {"base": df.groupby("d")["y_raw"].rank(pct=True).values}
    for name, clip in [("t1", 0.05), ("t1_c3", 0.03), ("t1_c10", 0.10)]:
        y[name] = (targets(df, clip)[0] - cost).values
    y["t1p"] = targets(df, 0.05)[0].values
    y["t2"] = targets(df, 0.05)[1].values

    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    picks, log = [], []
    for k in range(len(FOLD_STARTS)):
        te_mask = ((df["d"] >= bounds[k]) & (df["d"] < bounds[k + 1])).values
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr_mask = df["d"].isin(train_days).values
        te = df[te_mask]
        picks.append(evaluate(te, -te["key_sort"].values, "gap_vol", 0))
        for v in variants:
            for s in range(a.seeds):
                t0 = time.time()
                pred, best = fit_predict(v, s, X[tr_mask], y[v][tr_mask], df["d"][tr_mask], X[te_mask])
                picks.append(evaluate(te, pred, v, s))
                log.append(dict(fold=FOLD_STARTS[k], variant=v, seed=s, trees=best))
                print(f"{FOLD_STARTS[k]} {v} seed {s}: 木 {best} 本、{time.time() - t0:.0f} 秒", flush=True)

    p = pd.concat(picks, ignore_index=True)
    p["ret_flat"] = p["w"] * (p["y_raw"] - 5.7 / 1e4)
    p["ret_liq"] = p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4)
    p.to_parquet(f"test/out/dt_wf_target_{a.tag}_picks.parquet", index=False)
    daily = p.groupby(["variant", "seed", "d"], as_index=False)[["ret_flat", "ret_liq"]].sum()
    daily.to_parquet(f"test/out/dt_wf_target_{a.tag}_daily.parquet", index=False)
    pd.DataFrame(log).to_csv(f"test/out/dt_wf_target_{a.tag}_trees.csv", index=False)
    print(f"完了: {len(daily):,} 行")


if __name__ == "__main__":
    main()
