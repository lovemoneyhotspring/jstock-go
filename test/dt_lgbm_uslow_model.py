"""米国小幅高の日（本番が LightGBM で並べる日）のモデルを、その日だけで学習する（LU）か、全日に日の種類の印を
足して学習する（LW）と、今の全日で学習したモデル（L）より良くなるか。

根拠: vault 20-research/2026-09-jp-daytrade-uslow-model.md（2026-09-26、ユーザの提案）。小幅高の日は L が G に勝つ唯一の日
（2026-09-jp-daytrade-remeasure-afford-err: +4,320 円/日、誤差込み t 2.08）。その日にギャップダウンする銘柄は市場に連れた下げでなく
個別の悪材料が多いはずで、全日のモデルはこの違いを平均でならしている。

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_uslow_model.py --prelim   # 予備（誤差なし＋9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_uslow_model.py --check    # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_uslow_model.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習: test/dt_prefer_lgbm_wf.py と同じ walk-forward（FOLD_STARTS 7 本・EMBARGO 5 日・KW・内部検証 250 日の early stopping）。
        目的変数・特徴量の作り方（日ごとの百分位）は同じ。検証期間 2019-09-17〜2026-09-15、12 月は除く。
  モデル（fold ごとに学習）:
    M0  今の特徴量 16 個、全日（ギャップ < 0 の候補）で学習
    MU  M0 と同じ特徴量、学習を米国小幅高の日（day_rules(us="spx") の us_low）だけに絞る
    MW  M0 ＋ 日の種類の印（us_low を 0/1、百分位に直さない）の 17 個、全日で学習
    内部検証（early stopping）は後ろ 250 日。ただし学習の日数の半分を超えない（MU の最初の fold は小幅高の日が約 250 日しかない）
  並べ方（小幅高の日だけ。それ以外の日はどの形も G = gap_vol で本番と同じなので差は 0）:
    L = M0、LU = MU、LW = MW の予測値の高い順。業種の上限 1。
  本番の形: 日の区分は本番（day_rules(us="spx", skip_months=(12,))）、単元の判定あり（DT_AFFORD=1）、規則 R（p 0.2%・
           資金 ÷ 7・10 位まで）で 1,000 万、I0 10 bp。小幅高の日は寄指 −1.5%（本番の preopen_limit_pct_us_low = 1.5）:
           始値のギャップ ≤ −1.5% の銘柄だけ始値で約定し、残りの枠は現金。
  気配の誤差: 判定は層にした既定の材料（8:59:52 の発注時の気配ほか）を day_boot で、第一・第二の層が 20 日以上あるときだけ。
             最終日は前日に固定。20 シード。
  判定（主）: 小幅高の日の LU − L と LW − L（シードで平均した日次の差）。2 本を試すので、次をすべて満たしたものを「効く」:
    1. 差 > 0 かつ誤差込みの t（tstat_err）≥ 2.3
    2. 誤差なし（見えるギャップ = 始値のギャップ）でも差 > 0
    3. 前半（2019-09-17〜2022-12-31）と後半（2023-01-01〜）の両方で差 > 0
    両方が効くなら誤差込みの t の大きい方。どちらも満たさなければ全日のモデルのまま閉じる。
  記述（採否に使わない）: 日の t、年ごと、小幅高の日の約定率、全日の年率と最大 DD、予備の数字。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402
from dt_preopen_sim import with_rule_rank  # noqa: E402

NEED_DAYS = 20
T_MIN = 2.3
LIMIT = -0.015      # 小幅高の日の寄指（前日終値 −1.5%）
SPLIT = pd.Timestamp("2023-01-01")
PRELIM_UNTIL = "2026-09-24"   # 予備の誤差の材料（slot 0859、7 日）
MODELS = {"M0": (False, False), "MU": (True, False), "MW": (False, True)}   # (小幅高の日だけで学習, 日の印)
VARIANTS = {"L": "M0", "LU": "MU", "LW": "MW"}


def feats(g, flag):
    x = ranked(raw_features(g), g["d"]).values
    return np.column_stack([x, g["us_low"].values.astype(float)]) if flag else x


def fit(tr, flag):
    tr = with_rule_rank(tr)
    X = feats(tr, flag)
    y = tr.groupby("d")["y_raw"].rank(pct=True).values
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[max(len(days) - INNER_VALID_DAYS, len(days) // 2)]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    return LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW).fit(X, y)


def material_days():
    keep, sim.MIN_SNAP_DAYS = sim.MIN_SNAP_DAYS, 0
    try:
        e = sim.error_frame("snap", "2026-09-11")
    finally:
        sim.MIN_SNAP_DAYS = keep
    first = e.loc[e["src"] != "085930", "d"]
    return first.nunique(), (pd.Timestamp(first.max()).strftime("%Y-%m-%d") if len(first) else None)


def run_once(g, models, gf, rules, uslow_days):
    """小幅高の日だけ並べて損益を出す（寄指 −1.5%: 始値のギャップ ≤ −1.5% だけ約定、残りは現金）。"""
    g = g[g["d"].isin(uslow_days)]
    gf = gf[g.index]
    by = {}
    for v, name in VARIANTS.items():
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf.values == k] = m.predict(feats(g[gf.values == k], MODELS[name][1]))
        by[v] = cap_and_rank(g, s)
    kappa = calib_kappa(cap_and_rank(g, -g["key_sort"].values), I0)
    out, fill = {}, {}
    for v, x in by.items():
        pn, fr = {}, []
        for d, y in x.groupby("d"):
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
            ok = y.loc[idx, "gap_true"].values <= LIMIT
            fr.append(ok.mean() if len(ok) else np.nan)
            pn[d] = pnl_day(y, idx[ok], w[ok], kappa) if ok.any() else 0.0
        out[v], fill[v] = pd.Series(pn), np.nanmean(fr)
    return out, fill


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--check", action="store_true", help="材料の日数を見るだけ")
    ap.add_argument("--prelim", action="store_true", help="予備: 誤差なし＋9/24 までの slot 0859（採否に使わない）")
    a = ap.parse_args()

    n, last = material_days()
    print(f"8:59:48 以降の材料のある日: {n} 日（最終 {last}）、判定に要る日数 {NEED_DAYS}")
    if a.check:
        print("回してよい" if n >= NEED_DAYS else f"まだ（あと {NEED_DAYS - n} 日）")
        return
    if a.prelim:
        os.environ["DT_ERR_UNTIL"] = PRELIM_UNTIL
        slot, seeds = "0859", min(a.seeds, 10)
        print("**予備: 誤差の材料は slot 0859・9/24 まで（7 日）。採否に使わない**")
    else:
        if n < NEED_DAYS:
            sys.exit(f"材料が {NEED_DAYS} 日に満たないので回さない（予備は --prelim）")
        os.environ["DT_ERR_UNTIL"] = last
        os.environ.pop("DT_ERR_SLOT", None)
        slot, seeds = sim.SNAP_SLOT, a.seeds

    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    rules_all = day_rules(df, us="spx")
    df["us_low"] = df["d"].map(rules_all["us_low"]).fillna(False).astype(bool)
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]

    models = {k: [] for k in MODELS}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name, (only_low, flag) in MODELS.items():
            models[name].append(fit(tr[tr["us_low"]] if only_low else tr, flag))
        print(f"{FOLD_STARTS[k]}: 木 " + "・".join(f"{nm} {models[nm][-1].n_estimators}" for nm in MODELS) +
              f" 本（小幅高の日 {tr[tr['us_low']]['d'].nunique()} / {tr['d'].nunique()} 日）", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    uslow_days = pd.DatetimeIndex(sorted(te.loc[te["us_low"], "d"].unique()))

    def series(g):
        out, fill = run_once(g, models, fold_of.loc[g.index], rules, uslow_days)
        return {v: s.reindex(uslow_days).fillna(0.0) for v, s in out.items()}, fill

    upper, fill0 = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    acc = {v: [] for v in VARIANTS}
    for seed in range(seeds):
        out, _ = series(sim.seen(te, sim.draw_errors(pools, te, seed)))
        for v in VARIANTS:
            acc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    allm = np.ones(len(uslow_days), bool)
    print(f"\n小幅高の日 {len(uslow_days)} 日（{uslow_days.min():%Y-%m-%d}〜{uslow_days.max():%Y-%m-%d}）、"
          f"資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・寄指 {LIMIT * 100:g}%・{seeds} シード（円/日）")
    print(f"  L の水準 {mean['L'].mean():+,.0f}（誤差なし {upper['L'].mean():+,.0f}）、"
          f"約定率（誤差なし）" + "・".join(f"{v} {fill0[v]:.0%}" for v in VARIANTS))
    passed = []
    for v in ("LU", "LW"):
        d = mean[v] - mean["L"]
        te_ = tstat_err(acc[v], acc["L"], allm)
        up = (upper[v] - upper["L"]).mean()
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        checks = [d.mean() > 0 and te_ >= T_MIN, up > 0, first > 0 and second > 0]
        print(f"  {v} − L: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、誤差なし {up:+,.0f}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f} → " + ("○" if all(checks) else "×"))
        yr = d.index.year
        print("    年ごと: " + " ".join(f"{y}:{d[yr == y].mean():+,.0f}" for y in sorted(set(yr))))
        if all(checks):
            passed.append((te_, v))
    if a.prelim:
        print("判定: （予備なので出さない）")
    else:
        print("判定: " + (f"{max(passed)[1]} が効く" if passed else "全日のモデルのまま閉じる"))
    for v in VARIANTS:
        print(f"  {v}: 小幅高の日の合計 年換算 {np.mean([x.sum() for x in acc[v]]) / 7:,.0f} 円・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in acc[v]]):.1f}%")


if __name__ == "__main__":
    main()
