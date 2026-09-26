"""順位への資金の配り方の本判定・段 2: 気配の誤差込み（事前登録: vault 20-research/2026-09-jp-daytrade-rank-allocation.md
「本判定の段取り」）。

  PYTHONPATH=test test/.venv/bin/python test/dt_rank_allocation_err.py --check    # 材料の日数
  PYTHONPATH=test test/.venv/bin/python test/dt_rank_allocation_err.py --prelim   # 予備（slot 0859・9/24 まで、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_rank_allocation_err.py [形 ...] [--ratios 0.001,0.002,0.003,0.005]   # 判定（材料 20 日から。段 1 を通った形を渡す）

土台は test/dt_lbz2_floor.py と同じ: walk-forward の BZ2（= LBZ2、FOLD_STARTS ごとに学習）、並べる前に売買代金 5 億未満を外す、
気配の誤差 20 シード（day_boot）、小幅高の日は寄指 −1.5%、12 月は除く。資金は本番の平常日の総額 700 万 × その日の倍率。
配り方は test/dt_rank_allocation.py の FORMS（R7 が今の形）。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_bag import MODELS, feats, fit, with_two_day  # noqa: E402
from dt_lgbm_gauss import SEEDS_OK  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, NEED_DAYS, PRELIM_UNTIL, SPLIT, material_days  # noqa: E402
from dt_nscale import calib_kappa, day_rules, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, EMBARGO, FOLD_STARTS, I0, LAST_DAY, cap_and_rank  # noqa: E402
from dt_rank_allocation import FORMS  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

TOTAL = 7_000_000
FLOOR = 5e8
T_MIN = 2.0


def alloc(y, p, weights, cap_total):
    """順位順に min(p × 売買代金, 総額 × 重み_k, 残り) を 1 単元の倍数で。1 単元が載らない銘柄は飛ばす（枠は使わない）。"""
    y = y.sort_values("rank")
    idx, amt, left, k = [], [], cap_total, 0
    for i, r in zip(y.index, y.itertuples()):
        if k >= len(weights):
            break
        lim = min(p * r.turnover_med, cap_total * weights[k])
        unit = r.price * 100
        if lim < unit:
            continue
        k += 1
        a = np.floor(min(lim, left) / unit) * unit
        if a > 0:
            idx.append(i)
            amt.append(a)
            left -= a
    return pd.Index(idx), np.array(amt)


def run_once(g, models, gf, rules, uslow, forms, ratios):
    kind = MODELS["BZ2"][0]
    s = np.empty(len(g))
    for k, m in enumerate(models):
        s[gf == k] = m.predict(feats(g[gf == k], kind))
    keep = g["turnover_med"].values >= FLOOR
    x = cap_and_rank(g[keep], s[keep])
    kappa = calib_kappa(cap_and_rank(g, -g["key_sort"].values), I0)
    out = {}
    for f in forms:
        form, ratio = f.split("@")[0], ratios[f]
        pn = {}
        for d, y in x.groupby("d"):
            idx, w = alloc(y, ratio, FORMS[form], TOTAL * rules.loc[d, "mult"])
            if d in uslow and len(idx):   # 小幅高の日は寄指 −1.5%
                ok = y.loc[idx, "gap_true"].values <= LIMIT
                idx, w = idx[ok], w[ok]
            pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
        out[f] = pd.Series(pn)
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("forms", nargs="*", help="段 1 を通った形（既定は R7 以外の全部）")
    ap.add_argument("--ratios", default="0.002", help="売買代金の比率（カンマ区切り。0.002 が今）")
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--check", action="store_true")
    ap.add_argument("--prelim", action="store_true")
    a = ap.parse_args()
    base = ["R7"] + [f for f in (a.forms or FORMS) if f != "R7"]
    ratios = {}
    for r in [float(x) for x in a.ratios.split(",")]:
        for f in base:
            ratios[f if r == 0.002 else f"{f}@{r * 100:g}%"] = r
    forms = list(ratios)

    n, last = material_days()
    print(f"8:59:48 以降の材料のある日: {n} 日（最終 {last}）、判定に要る日数 {NEED_DAYS}")
    if a.check:
        print("回してよい" if n >= NEED_DAYS else f"まだ（あと {NEED_DAYS - n} 日）")
        return
    if a.prelim:
        os.environ["DT_ERR_UNTIL"] = PRELIM_UNTIL
        slot, seeds = "0859", min(a.seeds, 10)
        print("**予備: 誤差の材料は slot 0859・9/24 まで。採否に使わない**")
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
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    kind, tkind, n_bag = MODELS["BZ2"]
    models = []
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)], kind, tkind, n_bag))
        print(f"{FOLD_STARTS[k]}: 木 {models[-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    lowm = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    normal = ~lowm
    uslow = set(alld[lowm])

    def series(g):
        out = run_once(g, models, fold_of.loc[g.index].values, rules, uslow, forms, ratios)
        return {f: s.reindex(alld).fillna(0.0) for f, s in out.items()}

    upper = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    acc = {f: [] for f in forms}
    for seed in range(seeds):
        out = series(sim.seen(te, sim.draw_errors(pools, te, seed)))
        for f in forms:
            acc[f].append(out[f])
        print(f"seed {seed}", flush=True)

    avg = lambda xs: pd.concat(xs, axis=1).mean(axis=1)  # noqa: E731
    mean = {f: avg(x) for f, x in acc.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、平常日 {normal.sum()} 日、総額 {TOTAL / 1e4:.0f} 万、{seeds} シード（円/日）")
    for f in forms:
        m = mean[f]
        print(f"  {f}: 平常日 {m[normal].mean():+,.0f}・全日 {m.mean():+,.0f}（誤差なし {upper[f].mean():+,.0f}）")
    # 本判定の選び方（2026-09-26 に改めた。vault ノートの「本判定での選び方」）: どの形も実績が無いので R7 を既定にせず、
    # 形 × 比率の組のうち誤差込みの全日の平均（20 シード平均）が最も高いものを採る。有意差は参考に出すだけ
    cand = list(forms)
    best = max(cand, key=lambda f: mean[f].mean())
    print("\n全日の平均（誤差込み、高い順）: " + "・".join(f"{f} {mean[f].mean():+,.0f}" for f in sorted(cand, key=lambda f: -mean[f].mean())))
    print(("（予備）" if a.prelim else "採用: ") + best)
    print("\n参考: 平常日の − 基準（形の比較は R7、比率の比較は同じ形の 0.2%）")
    for f in forms:
        if f == "R7":
            continue
        ref = f.split("@")[0] if "@" in f else "R7"
        d = (mean[f] - mean[ref])[normal]
        te_ = tstat_err(acc[f], acc[ref], normal)
        pos = sum((x - r)[normal].mean() > 0 for x, r in zip(acc[f], acc[ref]))
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        checks = [d.mean() > 0 and te_ >= T_MIN, pos >= SEEDS_OK * seeds, first > 0 and second > 0]
        if "@" in f:   # 比率の比較は誤差なしでも正であること（事前登録）
            checks.append((upper[f] - upper[ref])[normal].mean() > 0)
        print(f"  {f}: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、正のシード {pos}/{seeds}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f}、誤差なし {(upper[f] - upper[ref])[normal].mean():+,.0f} → "
              + ("（予備）" if a.prelim else ("○" if all(checks) else "×")) + " " + "".join("○" if c else "×" for c in checks))


if __name__ == "__main__":
    main()
