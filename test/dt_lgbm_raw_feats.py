"""LightGBM に特徴量を順位化せず生の値で渡し、教師データを生の bp（寄→引）にしたら、今（日ごとの順位で入れて日ごとの順位を学ぶ）
より良くなるか。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md（2026-09-26、ユーザの提案。教師データを生の bp にするのもユーザの指定）。
順位にするとギャップの深さの絶対値が消える（静かな日の −3% と荒れた日の −8% が同じ 1 位）。利益源は深いギャップ
（jp-gap-depth-is-the-edge）で、荒れた日ほど深い銘柄が増えて効く（2026-09-jp-daytrade-nisa-regime の追記）。
近い先行: 日単位の外部要因を生で足すと悪化（2026-09-jp-daytrade-ml-external-factors の J）、順位の特徴量のまま目的変数に
幅を残すと base に届かない（2026-09-jp-daytrade-ml-target-magnitude、差 −2.43 bp・t −1.02）。生の特徴量と生の bp の組は初めて。

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_raw_feats.py --prelim   # 予備（誤差なし＋9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_raw_feats.py --check    # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_raw_feats.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習: test/dt_prefer_lgbm_wf.py と同じ walk-forward（FOLD_STARTS 7 本・EMBARGO 5 日・KW・内部検証 250 日の early stopping）、
        全日のギャップ < 0 の候補で学習。検証期間 2019-09-17〜2026-09-15、12 月は除く。
  モデル（fold ごとに学習、L2 回帰）:
    M0  今: 特徴量 16 個を日ごとの百分位に、目的変数は寄→引の日ごとの百分位
    MR  特徴量 16 個を生の値（dt_lgbm_train.raw_features、±inf は欠け）、目的変数は生の bp（寄→引 × 1e4、刈らない・日の平均も引かない）
    MB  特徴量 = 百分位 16 個 ＋ 生の値 16 個、目的変数は MR と同じ生の bp
  並べ方: L = M0、LR = MR、LB = MB の予測値の高い順、業種の上限 1。
  本番の形: 日の区分は本番（day_rules(us="spx", skip_months=(12,))）、単元の判定あり、規則 R（p 0.2%・資金 ÷ 7・10 位まで）で
           1,000 万、I0 10 bp。米国小幅高の日は寄指 −1.5%（始値のギャップ ≤ −1.5% だけ約定、残りは現金）、それ以外の日は寄成。
  気配の誤差: 判定は層にした既定の材料（8:59:52 の発注時の気配ほか）を day_boot で、第一・第二の層が 20 日以上あるときだけ。
             最終日は前日に固定。20 シード。
  判定（主）: 米国小幅高の日（本番が LightGBM で並べる日）の LR − L と LB − L。2 本なので、次をすべて満たしたものを「効く」:
    1. 差 > 0 かつ誤差込みの t（tstat_err）≥ 2.3
    2. 誤差なし（見えるギャップ = 始値のギャップ）でも差 > 0
    3. 前半（2019-09-17〜2022-12-31）と後半（2023-01-01〜）の両方で差 > 0
    両方が効くなら誤差込みの t の大きい方。どちらも満たさなければ今のモデルのまま閉じる。
  記述（採否に使わない）: 平常日（それ以外の日）の L − G・LR − G・LB − G（G = gap_vol、6 の並べ方の判断材料）、日の t、年ごと、
                         選んだ銘柄のギャップの深さ、予備の数字。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, NEED_DAYS, PRELIM_UNTIL, SPLIT, T_MIN, material_days  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402
from dt_preopen_sim import with_rule_rank  # noqa: E402

MODELS = {"M0": ("rank", "rank"), "MR": ("raw", "bp"), "MB": ("both", "bp")}   # (特徴量, 目的変数)
VARIANTS = {"L": "M0", "LR": "MR", "LB": "MB"}


def feats(g, kind):
    raw = raw_features(g).replace([np.inf, -np.inf], np.nan)
    if kind == "raw":
        return raw.values
    rk = ranked(raw, g["d"]).values
    return rk if kind == "rank" else np.column_stack([rk, raw.values])


def fit(tr, kind, target):
    tr = with_rule_rank(tr)
    X = feats(tr, kind)
    y = tr.groupby("d")["y_raw"].rank(pct=True).values if target == "rank" else tr["y_raw"].values * 1e4
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    return LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW).fit(X, y)


def run_once(g, models, gf, rules, uslow):
    by = {"G": cap_and_rank(g, -g["key_sort"].values)}
    for v, name in VARIANTS.items():
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf.values == k] = m.predict(feats(g[gf.values == k], MODELS[name][0]))
        by[v] = cap_and_rank(g, s)
    kappa = calib_kappa(by["G"], I0)
    out, depth = {}, {}
    for v, x in by.items():
        pn, dp = {}, []
        for d, y in x.groupby("d"):
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
            if d in uslow:   # 小幅高の日は寄指 −1.5%
                ok = y.loc[idx, "gap_true"].values <= LIMIT
                idx, w = idx[ok], w[ok]
            pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
            dp.append(y.loc[idx, "gap_true"].mean() if len(idx) else np.nan)
        out[v], depth[v] = pd.Series(pn), np.nanmean(dp)
    return out, depth


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
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    models = {k: [] for k in MODELS}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name, (kind, target) in MODELS.items():
            models[name].append(fit(tr, kind, target))
        print(f"{FOLD_STARTS[k]}: 木 " + "・".join(f"{nm} {models[nm][-1].n_estimators}" for nm in MODELS) + " 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    lowm = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    uslow = set(alld[lowm])

    def series(g):
        out, depth = run_once(g, models, fold_of.loc[g.index], rules, uslow)
        return {v: s.reindex(alld).fillna(0.0) for v, s in out.items()}, depth

    upper, depth0 = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    names = ["G", *VARIANTS]
    acc = {v: [] for v in names}
    for seed in range(seeds):
        out, _ = series(sim.seen(te, sim.draw_errors(pools, te, seed)))
        for v in names:
            acc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in acc.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（米国小幅高の日 {lowm.sum()} 日）、"
          f"資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{seeds} シード（円/日）")
    print(f"  小幅高の日の L の水準 {mean['L'][lowm].mean():+,.0f}（誤差なし {upper['L'][lowm].mean():+,.0f}）、"
          "選んだ銘柄の始値のギャップ（誤差なし、平均）" + "・".join(f"{v} {depth0[v] * 100:.2f}%" for v in names))
    passed = []
    for v in ("LR", "LB"):
        d = (mean[v] - mean["L"])[lowm]
        te_ = tstat_err(acc[v], acc["L"], lowm)
        up = (upper[v] - upper["L"])[lowm].mean()
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        checks = [d.mean() > 0 and te_ >= T_MIN, up > 0, first > 0 and second > 0]
        print(f"  {v} − L（小幅高の日）: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、誤差なし {up:+,.0f}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f} → " + ("○" if all(checks) else "×"))
        yr = d.index.year
        print("    年ごと: " + " ".join(f"{y}:{d[yr == y].mean():+,.0f}" for y in sorted(set(yr))))
        if all(checks):
            passed.append((te_, v))
    print("判定: （予備なので出さない）" if a.prelim else
          "判定: " + (f"{max(passed)[1]} が効く" if passed else "今のモデルのまま閉じる"))
    print("\n記述（採否に使わない）: 平常日の − G")
    for v in VARIANTS:
        d = (mean[v] - mean["G"])[~lowm]
        print(f"  {v} − G: {d.mean():+,.0f}（誤差込み t {tstat_err(acc[v], acc['G'], ~lowm):+.2f}）、"
              f"誤差なし {(upper[v] - upper['G'])[~lowm].mean():+,.0f}")
    for v in names:
        print(f"  {v}: 全日の年率 {np.mean([x.mean() * 245 / CAP * 100 for x in acc[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in acc[v]]):.1f}%")


if __name__ == "__main__":
    main()
