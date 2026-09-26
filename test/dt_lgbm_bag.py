"""LightGBM を乱数の種違いで 5 本学習して平均し（bagging）、学習の揺れを抑えて教師データの z・正規分位点を測り直す。2 日続落も足す。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記「正規分位点の教師データを伸ばす」（2026-09-26 予備、test/dt_lgbm_gauss.py）。
理屈の上で同じ木になるはずの LGG と LYG が小幅高の日に 1,600 円/日ずれ、学習の揺れが比べたい差と同じ大きさだった。
平常日は LYZ（順位の特徴量・z の教師データ）が gap_vol に +3,204（t 1.34）で、6 条件中 t 以外を満たした。
2 日続落（前日・前々日とも終値が下げ）は 9/25 に規則の優先として不採用（gap_vol の日だけ t 0.66、test/dt_rsi2_deep.py の A）。
並べ方が LightGBM に変わると効き方も変わりうるので、特徴量と優先の 2 つの形で入れ直す（ユーザの提案）。

  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_bag.py --prelim   # 予備（誤差なし＋9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_bag.py --check    # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_bag.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習・本番の形・気配の誤差・主 B の条件は test/dt_lgbm_gauss.py と同じ（walk-forward 7 fold・L2 回帰・規則 R ÷7・10 位・1,000 万・
  小幅高の日は寄指 −1.5%・判定は 8:59:52 の層の材料 20 日を day_boot で 20 シード）。
  bagging: 木の本数は random_state 0 の early stopping で決め、random_state 0〜4 の 5 本を全期間で学習して予測を平均する
           （KW の subsample・colsample_bytree 0.8 で本ごとに違う木になる）。
  2 日続落: r_d1・r_d2（前日・前々日の終値の騰落、dt_three_day.lag_features）と A = 両方 < 0。
  モデル（比べる形）:
    L     順位・順位、1 本（今の本番）
    LB    順位・順位、bagging
    LBZ   順位・z（±3）、bagging
    LBG   順位・正規分位点、bagging
    LBZ2  順位（16 ＋ r_d2・A の 2 列、日ごとの百分位）・z、bagging
    LBZA  LBZ の並びで、業種の上限のあとの 20 位までの中で A を先に（dt_rsi2_deep の A と同じ優先）
  判定 主 A（小幅高の日、− L）: LB・LBZ・LBG・LBZ2・LBZA の 5 本。誤差込みの t ≥ 2.6 かつ差 > 0、誤差なしでも差 > 0、前半・後半とも差 > 0。
  判定 主 B（平常日、− G）: 同じ 5 本に dt_lgbm_gauss の条件 1〜6 を当てる（1 の t ≥ 2.6、6 は L − G を上回る）。
  どちらも複数なら誤差込みの t の大きい方。記述（採否に使わない）: bagging で揺れが縮んだか（LB − L）、木の本数、年率と最大 DD、年ごと。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_gauss import SCALES, SEEDS_OK, target  # noqa: E402
from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, NEED_DAYS, PRELIM_UNTIL, SPLIT, material_days  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
from dt_three_day import lag_features  # noqa: E402
from dt_wf_target import liq_cost_bp  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402
from dt_preopen_sim import with_rule_rank  # noqa: E402

T_MIN = 2.6
N_BAG = 5
POOL = 20
MODELS = {"M0": ("rank", "rank", 1), "B0": ("rank", "rank", N_BAG), "BZ": ("rank", "z", N_BAG),
          "BG": ("rank", "gauss", N_BAG), "BZ2": ("rank2", "z", N_BAG)}
VARIANTS = {"L": "M0", "LB": "B0", "LBZ": "BZ", "LBG": "BG", "LBZ2": "BZ2", "LBZA": "BZ"}
JUDGE = ("LB", "LBZ", "LBG", "LBZ2", "LBZA")


def with_two_day(df):
    days = np.sort(df["d"].unique())
    df = df.assign(d0=df["d"].map(pd.Series(days[:-1], index=days[1:])))
    df = df.merge(lag_features()[["code", "d0", "r_d1", "r_d2"]], on=["code", "d0"], how="left")
    df["A"] = (df["r_d1"] < 0) & (df["r_d2"] < 0)
    return df


def feats(g, kind):
    raw = raw_features(g).replace([np.inf, -np.inf], np.nan)
    if kind == "rank2":
        raw = raw.assign(r_d2=g["r_d2"].values, A=g["A"].astype(float).values)
    return ranked(raw, g["d"]).values


class Bag:
    def __init__(self, ms):
        self.ms = ms
        self.n_estimators = ms[0].n_estimators

    def predict(self, X):
        return np.mean([m.predict(X) for m in self.ms], axis=0)


def fit(tr, kind, tkind, n_bag):
    tr = with_rule_rank(tr)
    X = feats(tr, kind)
    y = target(tr, tkind)
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    n = m.best_iteration_ or 200
    return Bag([LGBMRegressor(n_estimators=n, **{**KW, "random_state": s}).fit(X, y) for s in range(n_bag)])


def prefer_a(g):
    g = g.assign(_p=np.where(g["rank"] <= POOL, np.where(g["A"].fillna(False), 0, 1), 2))
    g = g.sort_values(["d", "_p", "rank"], kind="mergesort")
    g["rank"] = g.groupby("d").cumcount() + 1
    return g


def run_once(g, models, gf, rules, uslow, liq=False):
    if liq:
        g = g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
    by, score = {"G": cap_and_rank(g, -g["key_sort"].values)}, {}
    for name in MODELS:
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf.values == k] = m.predict(feats(g[gf.values == k], MODELS[name][0]))
        score[name] = s
    for v, name in VARIANTS.items():
        by[v] = cap_and_rank(g, score[name])
    by["LBZA"] = prefer_a(by["LBZA"])
    kappa = calib_kappa(by["G"], I0)
    out = {}
    for v, x in by.items():
        pn = {}
        for d, y in x.groupby("d"):
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
            if d in uslow:   # 小幅高の日は寄指 −1.5%
                ok = y.loc[idx, "gap_true"].values <= LIMIT
                idx, w = idx[ok], w[ok]
            pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
        out[v] = pd.Series(pn)
    return out


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
    df = with_two_day(df)
    print(f"2 日続落の割合 {df['A'].mean() * 100:.1f}%（r_d2 の欠け {df['r_d2'].isna().mean() * 100:.1f}%）")
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    models = {k: [] for k in MODELS}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name, (kind, tkind, n_bag) in MODELS.items():
            models[name].append(fit(tr, kind, tkind, n_bag))
        print(f"{FOLD_STARTS[k]}: 木 " + "・".join(f"{nm} {models[nm][-1].n_estimators}" for nm in MODELS) + " 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    lowm = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    normal = ~lowm
    uslow = set(alld[lowm])

    def series(g, liq=False):
        out = run_once(g, models, fold_of.loc[g.index], rules, uslow, liq)
        return {v: s.reindex(alld).fillna(0.0) for v, s in out.items()}

    upper = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    names = ["G", *VARIANTS]
    acc = {sc: {v: [] for v in names} for sc in (1.0,) + SCALES}
    liqc = {v: [] for v in names}
    for seed in range(seeds):
        e = sim.draw_errors(pools, te, seed)
        for sc in acc:
            out = series(sim.seen(te, e * sc))
            for v in names:
                acc[sc][v].append(out[v])
        out = series(sim.seen(te, e), liq=True)
        for v in names:
            liqc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    avg = lambda xs: pd.concat(xs, axis=1).mean(axis=1)  # noqa: E731
    base = acc[1.0]
    mean = {v: avg(x) for v, x in base.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（米国小幅高の日 {lowm.sum()} 日・平常日 "
          f"{normal.sum()} 日）、資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{seeds} シード・bagging {N_BAG} 本（円/日）")
    print(f"  小幅高の日の L の水準 {mean['L'][lowm].mean():+,.0f}（誤差なし {upper['L'][lowm].mean():+,.0f}）")

    print("\n主 A: 小幅高の日の − L")
    passed_a = []
    for v in JUDGE:
        d = (mean[v] - mean["L"])[lowm]
        te_ = tstat_err(base[v], base["L"], lowm)
        up = (upper[v] - upper["L"])[lowm].mean()
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        ok = d.mean() > 0 and te_ >= T_MIN and up > 0 and first > 0 and second > 0
        print(f"  {v} − L: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、誤差なし {up:+,.0f}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f} → {'○' if ok else '×'}")
        if ok:
            passed_a.append((te_, v))

    print("\n主 B: 平常日の − G")
    lq = {v: avg(x) for v, x in liqc.items()}
    ref = (mean["L"] - mean["G"])[normal].mean()
    passed_b = []
    for v in ["L", *JUDGE]:
        d = (mean[v] - mean["G"])[normal]
        te_ = tstat_err(base[v], base["G"], normal)
        pos = sum((x - g_)[normal].mean() > 0 for x, g_ in zip(base[v], base["G"]))
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        scaled = [(avg(acc[sc][v]) - avg(acc[sc]["G"]))[normal].mean() for sc in SCALES]
        d_lq = (lq[v] - lq["G"])[normal].mean()
        up = (upper[v] - upper["G"])[normal].mean()
        checks = [d.mean() > 0 and te_ >= T_MIN, pos >= SEEDS_OK * seeds, first > 0 and second > 0,
                  all(s > 0 for s in scaled), d_lq > 0, d.mean() > ref]
        mark = "（今）" if v == "L" else ("○" if all(checks) else "×")
        print(f"  {v} − G: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、正のシード {pos}/{seeds}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f}、誤差 0.8/1.2 倍 {scaled[0]:+,.0f}/{scaled[1]:+,.0f}、"
              f"流動性コスト込み {d_lq:+,.0f}、誤差なし {up:+,.0f} → {mark} "
              + "".join("○" if c else "×" for c in checks))
        yr = d.index.year
        print("    年ごと: " + " ".join(f"{y}:{d[yr == y].mean():+,.0f}" for y in sorted(set(yr))))
        if v != "L" and all(checks):
            passed_b.append((te_, v))
    if a.prelim:
        print("\n判定: （予備なので出さない）")
    else:
        print("\n判定 A: " + (f"{max(passed_a)[1]} が効く" if passed_a else "今のモデルのまま"))
        print("判定 B: " + (f"平常日も {max(passed_b)[1]} で並べる" if passed_b else "平常日は gap_vol のまま"))

    print("\n記述（採否に使わない）")
    for v in names:
        print(f"  {v}: 全日の年率 {np.mean([x.mean() * 245 / CAP * 100 for x in base[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in base[v]]):.1f}%")


if __name__ == "__main__":
    main()
