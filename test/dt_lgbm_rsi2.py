"""米国小幅高の日（本番が LightGBM で並べる日）に、RSI(2) を LightGBM と組み合わせたら良くなるか。

根拠: vault 20-research/2026-09-jp-daytrade-remeasure-afford-err.md（LF は RSI(2) を日ごとの百分位に直して入れていて、
「≤ 10」の水準が見えていなかった。ユーザの提案で、RSI(2) の値をそのまま入れる）

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_rsi2.py [--seeds 10]

事前登録（2026-09-25 夜、結果を見る前に固定。この docstring を commit してから回す）:
  位置づけ: 同じ候補表での RSI(2) の探索の 6 本目（割り引く）。
  学習: test/dt_prefer_lgbm_wf.py と同じ walk-forward（FOLD_STARTS 7 本・EMBARGO 5 日・KW・内部検証 250 日の early stopping）。
        検証期間 2019-09-17〜2026-09-15、12 月は除く。
  モデル:
    M0  今の特徴量 16 個（日ごとの百分位）
    MR  M0 ＋ RSI(2) の値（0〜100、百分位に直さない。欠けは欠けのまま）。木が閾値を学ぶ
        【2026-09-25 夜 決め直し、回す前】当初は定番の区切り 5/10/20/30/70 の段（MB・LB）だったが、木はもともと値の
        閾値で分岐するので区切らずに値を入れる（ユーザ判断）。段の案は回していない
  並べ方:
    L    M0 の予測値の高い順
    LP2  L のあと、業種の上限を通る上位 20 位の中で RSI(2) ≤ 10 を先に（LP のストキャス RSI を RSI(2) に替えたもの）
    LR   MR の予測値の高い順
    G    参考: gap_vol
  本番の形: 日の区分は本番（S&P500 0〜+1% かつ VIX ≤ 24、day_rules(us="spx", skip_months=(12,))）、単元の判定あり
           （DT_AFFORD=1）、業種の上限 1、規則 R（p 0.2%・資金 ÷ 7・10 位まで）で 1,000 万、I0 10 bp。
           気配の誤差は slot 0859・2026-09-11〜09-24（7 日に固定）を day_boot で引く、10 シード。
  判定（主）: 米国小幅高の日で LP2 − L と LR − L（シードで平均した日次の差）。2 本を試すので t ≥ 2.3 かつ差 > 0、
             さらに誤差なし（見えるギャップ = 始値のギャップ）でも同じ日の差が > 0 なら「効く」。
  記述（採否に使わない）: 誤差込みの t、それ以外の日の同じ差、年ごと、上位 10 本の RSI(2) ≤ 10 の占有。
"""

import argparse
import os

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）
os.environ["DT_ERR_UNTIL"] = "2026-09-24"

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_preopen_sim import draw_errors, error_pools, seen, with_rule_rank  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, POOL, RMAX, cap_and_rank  # noqa: E402
from dt_rsi_family import family_features  # noqa: E402

RSI2_MAX = 10


def feats(g, extra):
    x = ranked(raw_features(g), g["d"]).values
    if extra:
        x = np.column_stack([x, g["rsi2"].values.astype(float)])   # 値のまま（百分位に直さない）
    return x


def fit(tr, extra):
    tr = with_rule_rank(tr)
    X = feats(tr, extra)
    y = tr.groupby("d")["y_raw"].rank(pct=True).values
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    return LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW).fit(X, y)


def prefer_rsi2(g):
    g = g.assign(_p=np.where(g["rank"] <= POOL, np.where((g["rsi2"] <= RSI2_MAX).fillna(False), 0, 1), 2))
    g = g.sort_values(["d", "_p", "rank"], kind="mergesort")
    g["rank"] = g.groupby("d").cumcount() + 1
    return g


def run_once(g, models, gf, rules, kappa=None):
    sc = {}
    for name, extra in (("M0", False), ("MR", True)):
        X = feats(g, extra)
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf == k] = m.predict(X[gf == k])
        sc[name] = s
    by = {"L": cap_and_rank(g, sc["M0"]), "LR": cap_and_rank(g, sc["MR"]), "G": cap_and_rank(g, -g["key_sort"].values)}
    by["LP2"] = prefer_rsi2(by["L"])
    kappa = calib_kappa(by["G"], I0) if kappa is None else kappa
    out = {v: pd.Series({d: pnl_day(y, *alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7), kappa)
                         for d, y in x.groupby("d")}) for v, x in by.items()}
    return out, by, kappa


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    a = ap.parse_args()

    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    days = np.array(sorted(df["d"].unique()))
    df["d0"] = df["d"].map(pd.Series(days[:-1], index=days[1:]))
    df = df.merge(family_features()[["code", "d0", "rsi2"]], on=["code", "d0"], how="left")
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]

    models = {"M0": [], "MR": []}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name, extra in (("M0", False), ("MR", True)):
            models[name].append(fit(tr, extra))
        print(f"{FOLD_STARTS[k]}: 木 M0 {models['M0'][-1].n_estimators}・MR {models['MR'][-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    uslow = pd.Series(rules.reindex(alld)["us_low"].fillna(False).values, index=alld)
    uslow = uslow.values & (alld.month != 12)

    # 誤差なし（判定の副条件）
    g0 = seen(te, np.zeros(len(te)))
    upper, by0, _ = run_once(g0, models, fold_of.loc[g0.index].values, rules)
    upper = {v: s.reindex(alld).fillna(0.0) for v, s in upper.items()}

    pools = error_pools("0859", "2026-09-11", mode="day_boot")
    variants = ["L", "LP2", "LR", "G"]
    acc = {v: [] for v in variants}
    for seed in range(a.seeds):
        g = seen(te, draw_errors(pools, te, seed))
        out, _, _ = run_once(g, models, fold_of.loc[g.index].values, rules)
        for v in variants:
            acc[v].append(out[v].reindex(alld).fillna(0.0))
        print(f"seed {seed}", flush=True)

    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（米国小幅高の日 {uslow.sum()} 日）、"
          f"資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{a.seeds} シード（円/日）")
    print("| 日 | L | LP2 − L（t／誤差込み／誤差なしの差） | LR − L（同） | G | L − G（t／誤差込み） |")
    print("|---|---|---|---|---|---|")
    for name, m in (("米国小幅高の日（主）", uslow), ("それ以外の日", ~uslow), ("全日", np.ones(len(alld), bool))):
        def c(a_, b_, up=True):
            d = (mean[a_] - mean[b_])[m]
            s = f"{d.mean():+,.0f}（{tstat(d):+.2f}／{tstat_err(acc[a_], acc[b_], m):+.2f}"
            return s + (f"／{(upper[a_] - upper[b_])[m].mean():+,.0f}）" if up else "）")
        print(f"| {name} | {mean['L'][m].mean():,.0f} | {c('LP2', 'L')} | {c('LR', 'L')} | {mean['G'][m].mean():,.0f} | "
              f"{c('L', 'G', False)} |")
    yr = alld.year
    print("\n米国小幅高の日の年ごとの差（円/日）: " + "、".join(
        f"{v} " + " ".join(f"{y}:{(mean[v] - mean['L'])[uslow & (yr == y)].mean():+,.0f}" for y in sorted(set(yr)))
        for v in ("LP2", "LR")))
    top = by0["L"][by0["L"]["rank"] <= 10]
    print(f"記述: 誤差なしの L の上位 10 本で RSI(2) ≤ {RSI2_MAX} の占有 {(top['rsi2'] <= RSI2_MAX).mean():.1%}")
    for v in variants:
        print(f"  {v}: 全日の年率 {np.mean([x.mean() * 245 / CAP * 100 for x in acc[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in acc[v]]):.1f}%")


if __name__ == "__main__":
    main()
