"""LightGBM の日に、ストキャス RSI の優先（後から並べ替える）と、オシレータを特徴量に足した再学習を、
walk-forward（学習期間の外）で比べる。

根拠: vault 20-research/2026-09-jp-daytrade-oscillator.md（Go のバックテストの LightGBM の日の差は
本番のモデル＝2026-09-16 まで学習の学習期間の中の数字で、使えなかった。ユーザの指摘で再学習して測る）

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_prefer_lgbm_wf.py [--seeds 10]

事前登録（2026-09-25、結果を見る前に固定。この docstring を commit してから回す）:
  学習: test/dt_rank_compare.py と同じ walk-forward。検証期間の頭 FOLD_STARTS（2019-09-17 から 1 年ずつ 7 本）の
        それぞれで、その日より前（EMBARGO 5 日を空ける）の候補だけで学習する。設定は dt_lgbm_train の KW・
        内部検証 250 日の early stopping（dt_rank_compare.fit と同じ）。検証期間は 2019-09-17〜2026-09-15（12 月は除く）。
  モデルは 2 つ、fold ごとに学習する:
    M0  今の特徴量（dt_lgbm_train.FEATS の 16 個）
    MF  M0 に RSI(2) とストキャス RSI(14,14) を足した 18 個（ほかは同じ。日ごとの百分位に直すのも同じ）
  並べ方（寄る前の気配の誤差を入れて見える候補を作る。誤差は slot 0859・2026-09-11 以降の実測を帯ごとに iid。
  並べた後に業種の上限 1 を掛け、規則 R（p 0.2%・資金 ÷ 7・10 位まで＝本番の max_positions）で 1,000 万を配る。
  損益は真の寄→引から FEE と値を動かすコスト（I0 10 bp）を引く。10 シード）
    L   M0 の予測値の高い順
    LP  L のあと、業種の上限を通る上位 20 位の中でストキャス RSI ≤ 0.2 を先に（本番の selection.preferOversold と同じ）
    LF  MF の予測値の高い順
    G   参考: gap_vol（見えるギャップ ÷ vol20）
    GP  参考: G のあと LP と同じ優先
  日の区分: 米国小幅高の日（本番は LightGBM で並べる日）＝ dt_nscale.day_rules の近似（ナスダック 0〜+1% かつショックでない）。
  判定（主）: 米国小幅高の日で LP − L と LF − L（シードで平均した日次の差）。2 本を試すので t ≥ 2.3 かつ差 > 0 なら
             「LightGBM の日に効く」。
  記述（採否に使わない）: それ以外の日の同じ差、GP − G、年ごと。
"""

import argparse

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat
from dt_preopen_sim import BANDS, error_pools, seen, with_rule_rank
from dt_rsi_family import family_features
from dt_wf_target import EMBARGO, FOLD_STARTS

CAND = "test/out/dt_candidates_wide.parquet"
LAST_DAY = "2026-09-15"
POOL, SRSI_MAX = 20, 0.2
RMAX, CAP, I0 = 10, 1e7, 10e-4


def feats(g, extra):
    x = raw_features(g)
    if extra:
        x = x.assign(rsi2=g["rsi2"].values, srsi=g["srsi"].values)
    return ranked(x, g["d"]).values


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


def cap_and_rank(g, score):
    """予測値の高い順（同値は既存規則の順位）に並べ、業種の上限 1 を掛けて rank を付ける。"""
    g = g.assign(_s=score).sort_values(["d", "_s", "rule_rank"], ascending=[True, False, True], kind="mergesort")
    g = g[g.groupby(["d", "sector"], dropna=False).cumcount() < 1].copy()
    g["rank"] = g.groupby("d").cumcount() + 1
    return g


def prefer(g):
    g = g.assign(_p=np.where(g["rank"] <= POOL, np.where((g["srsi"] <= SRSI_MAX).fillna(False), 0, 1), 2))
    g = g.sort_values(["d", "_p", "rank"], kind="mergesort")
    g["rank"] = g.groupby("d").cumcount() + 1
    return g


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
    df = df.merge(family_features()[["code", "d0", "rsi2", "srsi"]], on=["code", "d0"], how="left")
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]

    models = {"M0": [], "MF": []}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name, extra in (("M0", False), ("MF", True)):
            models[name].append(fit(tr, extra))
        print(f"{FOLD_STARTS[k]}: 木 M0 {models['M0'][-1].n_estimators}・MF {models['MF'][-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te)
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    pools = error_pools("0859", "2026-09-11")
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    variants = ["L", "LP", "LF", "G", "GP"]
    acc = {v: [] for v in variants}
    for seed in range(a.seeds):
        rng = np.random.default_rng(seed)
        e = np.zeros(len(te))
        for b, pool in enumerate(pools):
            if (band == b).any():
                e[band == b] = rng.choice(pool, size=int((band == b).sum()))
        g = seen(te, e)
        gf = fold_of.loc[g.index].values
        sc = {}
        for name, extra in (("M0", False), ("MF", True)):
            X = feats(g, extra)
            s = np.empty(len(g))
            for k, m in enumerate(models[name]):
                s[gf == k] = m.predict(X[gf == k])
            sc[name] = s
        ranked_by = {"L": cap_and_rank(g, sc["M0"]), "LF": cap_and_rank(g, sc["MF"]), "G": cap_and_rank(g, -g["key_sort"].values)}
        ranked_by["LP"], ranked_by["GP"] = prefer(ranked_by["L"]), prefer(ranked_by["G"])
        kappa = calib_kappa(ranked_by["G"], I0)
        for v in variants:
            x = ranked_by[v]
            s = pd.Series({d: pnl_day(y, *alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7), kappa)
                           for d, y in x.groupby("d")})
            acc[v].append(s.reindex(alld).fillna(0.0))
        print(f"seed {seed}", flush=True)

    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    uslow = rules.reindex(alld)["skip"].fillna(False).values
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（米国小幅高の日 {uslow.sum()} 日）、"
          f"資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{a.seeds} シード（円/日）")
    print("| 日 | L | LP − L（t） | LF − L（t） | G | GP − G（t） | L − G（t） |")
    print("|---|---|---|---|---|---|---|")
    for name, m in (("米国小幅高の日（主）", uslow), ("それ以外の日", ~uslow), ("全日", np.ones(len(alld), bool))):
        c = lambda a_, b_: f"{(mean[a_] - mean[b_])[m].mean():+,.0f}（{tstat((mean[a_] - mean[b_])[m]):+.2f}）"
        print(f"| {name} | {mean['L'][m].mean():,.0f} | {c('LP', 'L')} | {c('LF', 'L')} | {mean['G'][m].mean():,.0f} | "
              f"{c('GP', 'G')} | {c('L', 'G')} |")
    yr = alld.year
    print("\n米国小幅高の日の年ごとの差（円/日）: " + "、".join(
        f"{v} " + " ".join(f"{y}:{(mean[v] - mean['L'])[uslow & (yr == y)].mean():+,.0f}" for y in sorted(set(yr)))
        for v in ("LP", "LF")))
    for v in variants:
        print(f"  {v}: 全日の年率 {np.mean([x.mean() * 245 / CAP * 100 for x in acc[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in acc[v]]):.1f}%")


if __name__ == "__main__":
    main()
