"""米国小幅高の日に、並べ方（今の 1 本 L / LBZ2＋売買代金 5 億）× 注文（寄成・寄指 −0.5 / −1.0 / −1.5%）× 指値で約定しない候補を
先に外すか、を比べる。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md（2026-09-26）。小幅高の日は LBZ2 が今の L に −731 円/日（誤差なし −1,281）。
LBZ2 は浅いギャップを選ぶ（平常日の平均 −1.06%、gap_vol −1.30%）ので、寄指 −1.5%（始値のギャップ ≤ −1.5% でだけ約定）では
選んだ銘柄の多くが約定せず、本数が減っているという読み。指値ごとに、見えるギャップが指値より浅い候補を並べる前に外せば、
資金を約定しうる銘柄に回せる。寄指の幅はユーザの指定で 3 つ、寄成も並べる。

  PYTHONPATH=test bash test/heavy.sh test/.venv/bin/python test/dt_uslow_order_grid.py --prelim   # 予備（slot 0859・9/24 まで、採否に使わない）
  PYTHONPATH=test bash test/heavy.sh test/.venv/bin/python test/dt_uslow_order_grid.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習: test/dt_lgbm_bag.py と同じ walk-forward 7 fold。L = M0（1 本、順位・順位）、Z2 = BZ2（bagging 5 本・2 日続落・z）。
  対象の日: 米国小幅高の日（day_rules(us="spx", skip_months=(12,)) の us_low）。資金・規則 R・業種の上限は dt_lgbm_bag と同じ。
  並べ方: L は今のまま、Z2F5 は売買代金 5 億未満を並べる前に外す（本番の LBZ2 と同じ）。
  注文: 寄成（すべて約定）、寄指 x ∈ {0.5, 1.0, 1.5}%（始値のギャップ ≤ −x のときだけ約定、約定しない分は現金）。
  先に外す（pre）: 寄指のとき、見えるギャップが −x より浅い候補を並べた後・建玉を割り当てる前に外す（順位は詰める）。
  形は 14 通り: {L, Z2F5} × {寄成, −0.5, −1.0, −1.5} × {外さない, 外す（寄指だけ）}。基準は今の本番 = L × 寄指 −1.5% × 外さない。
  滑りの係数 kappa は全日の誤差なしの G（gap_vol）の上位 3 本で 1 度だけ合わせて固定（小幅高の日だけで合わせない）。
  判定（主）: 基準との差（13 本）。tstat_nested（day_boot、20 シード × 2 本）≥ 3.0（5% ÷ 13、両側）かつ差 > 0、誤差なしでも差 > 0、
    前半（〜2022）・後半（2023〜）とも差 > 0 をすべて満たしたものを「効く」。複数なら t の大きい方。どれも満たさなければ今のまま。
  記述（採否に使わない）: 各形の水準、約定率、1 日の建玉、tstat_err、年ごと。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_bag import MODELS, feats, fit, with_two_day  # noqa: E402
from dt_lgbm_uslow_model import NEED_DAYS, PRELIM_UNTIL, SPLIT, material_days  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, pnl_day, tstat_err, tstat_nested  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

T_MIN = 3.0
FLOOR = 5e8
LIMITS = {"MOO": None, "L05": 0.005, "L10": 0.010, "L15": 0.015}
BASE = "L|L15"
REPS = 2


def variants():
    out = []
    for r in ("L", "Z2F5"):
        for o, x in LIMITS.items():
            out.append((f"{r}|{o}", r, x, False))
            if x is not None:
                out.append((f"{r}|{o}|pre", r, x, True))
    return out


VARIANTS = variants()


def run_once(g, models, gf, rules, kappa):
    """形ごとの日次損益・約定率・建玉（小幅高の日だけ渡す）。"""
    sc = {}
    for name in ("M0", "BZ2"):
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf == k] = m.predict(feats(g[gf == k], MODELS[name][0]))
        sc[name] = s
    keep = g["turnover_med"].values >= FLOOR
    by = {"L": cap_and_rank(g, sc["M0"]), "Z2F5": cap_and_rank(g[keep], sc["BZ2"][keep])}
    out = {}
    for v, r, x, pre in VARIANTS:
        pn, fill, amt = {}, [], {}
        for d, y in by[r].groupby("d"):
            if pre:
                y = y[y["gap"] <= -x].sort_values("rank")
                y = y.assign(rank=np.arange(1, len(y) + 1))
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7) if len(y) else (y.index[:0], np.zeros(0))
            if x is not None and len(idx):
                ok = y.loc[idx, "gap_true"].values <= -x
                fill.append(ok.mean())
                idx, w = idx[ok], w[ok]
            pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
            amt[d] = float(np.sum(w))
        out[v] = (pd.Series(pn), np.mean(fill) if fill else 1.0, pd.Series(amt))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--prelim", action="store_true", help="予備: slot 0859・9/24 まで（採否に使わない）")
    a = ap.parse_args()
    n, last = material_days()
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
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    models = {"M0": [], "BZ2": []}
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        tr = df[df["d"].isin(train_days) & (df["gap"] < 0)]
        for name in models:
            kind, tkind, n_bag = MODELS[name]
            models[name].append(fit(tr, kind, tkind, n_bag))
        print(f"{FOLD_STARTS[k]}: 木 M0 {models['M0'][-1].n_estimators}・BZ2 {models['BZ2'][-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    g0 = sim.seen(te, np.zeros(len(te)))
    kappa = calib_kappa(cap_and_rank(g0, -g0["key_sort"].values), I0)
    uslow = rules.index[rules["us_low"].fillna(False).astype(bool)]
    te = te[te["d"].isin(uslow)]
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    allm = np.ones(len(alld), dtype=bool)

    def series(g):
        out = run_once(g, models, fold_of.loc[g.index].values, rules, kappa)
        return {v: (s.reindex(alld).fillna(0.0), f, am.reindex(alld).fillna(0.0)) for v, (s, f, am) in out.items()}

    upper = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    names = [v for v, *_ in VARIANTS]
    acc = {v: [[] for _ in range(seeds)] for v in names}
    fills = {v: [] for v in names}
    amts = {v: [] for v in names}
    for seed in range(seeds):
        for rep in range(REPS):
            out = series(sim.seen(te, sim.draw_errors(pools, te, seed, rep)))
            for v in names:
                acc[v][seed].append(out[v][0])
                fills[v].append(out[v][1])
                amts[v].append(out[v][2].mean())
        print(f"seed {seed}", flush=True)

    print(f"\n米国小幅高の日 {len(alld)} 日（{alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}）、1,000 万・{RMAX} 位まで、"
          f"{seeds} シード × {REPS} 本（円/日）。基準 {BASE}（今の本番）")
    base_mean = np.mean([s.mean() for r in acc[BASE] for s in r])
    print(f"  基準の水準 {base_mean:+,.0f}（誤差なし {upper[BASE][0].mean():+,.0f}）")
    passed = []
    for v in names:
        lvl = np.mean([s.mean() for r in acc[v] for s in r])
        head = f"  {v:<13} 水準 {lvl:+,.0f}（誤差なし {upper[v][0].mean():+,.0f}）、約定率 {np.mean(fills[v]) * 100:.0f}%、" \
               f"1 日の建玉 {np.mean(amts[v]) / 1e4:,.0f} 万"
        if v == BASE:
            print(head + " ← 基準")
            continue
        t, d, _, _, _ = tstat_nested(acc[v], acc[BASE], allm)
        t_old = tstat_err([r[0] for r in acc[v]], [r[0] for r in acc[BASE]], allm)
        x = np.mean([[(ai - bi).values for ai, bi in zip(ra, rb)] for ra, rb in zip(acc[v], acc[BASE])], axis=(0, 1))
        x = pd.Series(x, index=alld)
        up = (upper[v][0] - upper[BASE][0]).mean()
        first, second = x[x.index < SPLIT].mean(), x[x.index >= SPLIT].mean()
        ok = d > 0 and t >= T_MIN and up > 0 and first > 0 and second > 0
        print(head + f"、差 {d:+,.0f}（t {t:+.2f}・tstat_err {t_old:+.2f}）、誤差なし {up:+,.0f}、前半 {first:+,.0f}・後半 {second:+,.0f} → "
              + ("○" if ok else "×"))
        yr = x.index.year
        print("      年ごと: " + " ".join(f"{y}:{x[yr == y].mean():+,.0f}" for y in sorted(set(yr))))
        if ok:
            passed.append((t, v))
    print("\n判定: （予備なので出さない）" if a.prelim else
          "\n判定: " + (f"{max(passed)[1]} が効く" if passed else "今のまま（L × 寄指 −1.5%）"))


if __name__ == "__main__":
    main()
