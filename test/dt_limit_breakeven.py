"""寄指の指値を「損益分岐の少し下」に固定で置いたら: 分岐点は各年の手前のデータだけで決める（拡大窓の walk-forward）。

根拠: vault 20-research/2026-09-jp-daytrade-limit-on-open.md の「事前登録 2」（2026-09-21）

  test/.venv/bin/python test/dt_limit_breakeven.py [--seeds 20] [--side-seeds 10] [--slot 0859] [--err-since 2026-09-11]
  test/.venv/bin/python test/dt_limit_breakeven.py --report-only

選定は B（今の本番: gap_vol 上位 3 本を寄成、米国小幅高は休む）と同じ。寄指は指値のギャップ −c を固定で置き、始値のギャップ < −c だけ約定。
  c*  その年より前の選定（誤差込み、全シード）を実際の始値のギャップで区切り、浅い側から見て平均が初めて正になる区間の上端
  Q   指値 = c* の 1 段深い格子点（判定はこの 1 本）。Q0 は c* ちょうど（参考）
誤差の入れ方・選定の規則は test/dt_limit_on_open.py と同じ。模擬は 2016-09 から回し（c* の材料）、損益は 2019-09-17 から数える。
"""

import argparse

import duckdb
import numpy as np
import pandas as pd

from dt_limit_on_open import CAPITAL, N_BASE, select, stats
from dt_preopen_sim import BANDS, ERR_SQL, seen
from dt_rank_compare import CAND, LAST_DAY, SEEN_FROM, US, draw, tstat
from dt_wf_target import EMBARGO, FOLD_STARTS

EDGES = [np.inf, 0.0, -0.005, -0.01, -0.015, -0.02, -0.03, -0.04, -0.05]  # 指値の格子（ギャップ）。inf は指値なし = 寄成
OUT = "test/out/dt_limit_breakeven_picks.parquet"
HYBRID_LIMIT = -0.005  # 折衷の探索で寄指の側に置く指値（探索で一番良かった固定の指値）


def label(c):
    return "寄成" if np.isinf(c) else f"{c * 100:g}%"


def breakeven(prior):
    """浅い側から区間ごとの平均を見て、初めて正になる区間の上端（c*）とその 1 段深い点（Q）。正の区間が無ければ寄成のまま。"""
    lo = EDGES[1:] + [-np.inf]
    means = [prior.loc[(prior["gap_true"] < hi) & (prior["gap_true"] >= l), "net"].mean() for hi, l in zip(EDGES, lo)]
    for i, m in enumerate(means):
        if m > 0:
            return EDGES[i], EDGES[min(i + 1, len(EDGES) - 1)], means
    return np.inf, np.inf, means


def report(picks):
    days = pd.DatetimeIndex(sorted(picks["d"].unique()))
    us = pd.read_parquet(US)[["date", "spx_ret1", "vix"]].rename(columns={"date": "d"})
    us["us_low"] = (us["spx_ret1"] >= 0) & (us["spx_ret1"] < 0.01) & (us["vix"].isna() | (us["vix"] <= 24))
    rest = set(us.loc[us["us_low"], "d"])
    picks = picks[~picks["d"].isin(rest)].copy()  # 休む日は全設定とも 0
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    all_days = days[days >= bounds[0]]
    unseen = all_days < SEEN_FROM
    print(f"\n{all_days[0]:%Y-%m-%d}〜{all_days[-1]:%Y-%m-%d}、{len(all_days)} 日（12 月を除く）、うち休む {sum(d in rest for d in all_days)} 日、流動性別コスト")

    def series(q, limit_of_day=None, fixed=None, market_below=-np.inf):
        """market_below: 見えるギャップがこれより深い銘柄は寄成（必ず約定）、残りは寄指。"""
        lim = fixed if fixed is not None else q["d"].map(limit_of_day)
        r = (q["w"] * q["net"] * ((q["gap_true"] < lim) | (q["gap"] < market_below))).groupby(q["d"]).sum()
        return r.reindex(all_days).fillna(0.0)

    for form in ("block", "block_x0.8"):
        pf = picks[picks["form"] == form]
        seeds = sorted(pf["seed"].unique())
        print(f"\n## 誤差の形 {form}（シード {len(seeds)} 本）")
        print("| 年（開始） | 手前の行数 | " + " | ".join(f"{label(h)} 未満" if i else "0% 以上" for i, h in enumerate(EDGES)) + " | c* | Q |")
        print("|---|---|" + "---|" * (len(EDGES) + 2))
        q_of, q0_of = {}, {}
        for k in range(len(FOLD_STARTS)):
            prior_days = days[days < bounds[k]][:-EMBARGO]
            prior = pf[pf["d"].isin(prior_days)]
            c0, c1, means = breakeven(prior)
            for d in all_days[(all_days >= bounds[k]) & (all_days < bounds[k + 1])]:
                q0_of[d], q_of[d] = c0, c1
            print(f"| {FOLD_STARTS[k]} | {len(prior):,} | " + " | ".join(f"{m * 1e4:+.0f}" for m in means) + f" | {label(c0)} | {label(c1)} |")
        print("（区間は上端で表示: 「0% 未満」は −0.5%〜0%、最後は −5% 未満。値は始値→引けのコスト後の平均 bp）")

        pe = pf[pf["d"].isin(all_days)]
        per_seed = {s: pe[pe["seed"] == s] for s in seeds}
        mean_of = lambda **kw: sum(series(per_seed[s], **kw) for s in seeds) / len(seeds)
        b = mean_of(fixed=np.inf)
        print(f"\nB（寄成・3 本）= {stats(b)[0]:+.2f} bp/日、t {stats(b)[1]:.2f}、Sharpe {stats(b)[2]:.2f}、最大 DD {stats(b)[3]:.1f}%")
        print("| 設定 | bp/日 | Sharpe | 最大 DD | B との差 | t | 未見 / 既見の差 | 差 > 0 のシード | 約定率 | 約定した側 / しなかった側（bp） |")
        print("|---|---|---|---|---|---|---|---|---|---|")
        rows = [("Q（c* の 1 段下、判定）", dict(limit_of_day=q_of)), ("Q0（c* ちょうど、参考）", dict(limit_of_day=q0_of))]
        rows += [(f"固定 {label(c)}（探索）", dict(fixed=c)) for c in EDGES[1:]]
        for name, kw in rows:
            x = mean_of(**kw)
            d = x - b
            pos = sum((series(per_seed[s], **kw) - series(per_seed[s], fixed=np.inf)).mean() > 0 for s in seeds)
            fill = pe["gap_true"] < (kw["fixed"] if "fixed" in kw else pe["d"].map(kw["limit_of_day"]))
            print(f"| {name} | {stats(x)[0]:+.2f} | {stats(x)[2]:.2f} | {stats(x)[3]:.1f}% | {d.mean() * 1e4:+.2f} | {tstat(d):.2f} |"
                  f" {d[unseen].mean() * 1e4:+.2f} / {d[~unseen].mean() * 1e4:+.2f} | {pos}/{len(seeds)} | {fill.mean() * 100:.0f}% |"
                  f" {pe.loc[fill, 'net'].mean() * 1e4:+.1f} / {pe.loc[~fill, 'net'].mean() * 1e4:+.1f} |")

        # 探索（事前登録なし、2026-09-21 追記）: 見えるギャップが深い銘柄は寄成、浅い銘柄だけ寄指（指値 −0.5%）
        print(f"\n折衷（探索）: 見えるギャップが s より深ければ寄成、浅ければ寄指（指値 {label(HYBRID_LIMIT)}）。差は「全部を寄指」との比較")
        print("| s | bp/日 | B との差（t） | 全部を寄指との差（t） | 寄成に回る割合 | うち指値より浅く寄った行 / その始値→引け（bp） |")
        print("|---|---|---|---|---|---|")
        allq = mean_of(fixed=HYBRID_LIMIT)
        for sg in (-0.01, -0.02, -0.03, -0.05):
            x = mean_of(fixed=HYBRID_LIMIT, market_below=sg)
            deep = pe[pe["gap"] < sg]
            miss = deep[deep["gap_true"] >= HYBRID_LIMIT]
            print(f"| {label(sg)} | {stats(x)[0]:+.2f} | {(x - b).mean() * 1e4:+.2f}（{tstat(x - b):.2f}） | {(x - allq).mean() * 1e4:+.2f}（{tstat(x - allq):.2f}） |"
                  f" {len(deep) / len(pe) * 100:.0f}% | {len(miss) / len(deep) * 100:.1f}% / {miss['net'].mean() * 1e4:+.1f} |")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--side-seeds", type=int, default=10)
    ap.add_argument("--slot", default="0859")
    ap.add_argument("--err-since", default="2026-09-11")
    ap.add_argument("--report-only", action="store_true")
    a = ap.parse_args()

    if not a.report_only:
        err = duckdb.sql(ERR_SQL, params=[a.slot, a.err_since, a.err_since]).df()
        print(f"誤差の実測: slot {a.slot}、{err['d'].nunique()} 日、{len(err):,} 行", flush=True)
        err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
        df = pd.read_parquet(CAND)
        df["price"] = df["o"]
        df["sector"] = df["sector"].fillna("")
        te = df[(df["d"] <= LAST_DAY) & (df["d"].dt.month != 12)].copy()
        band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
        day_idx = te["d"].rank(method="dense").astype(int).values - 1
        res = []
        for form, scale, n in (("block", 1.0, a.seeds), ("block_x0.8", 0.8, a.side_seeds)):
            for s in range(n):
                g = seen(te, draw(np.random.default_rng(s), err, band, day_idx, "block") * scale)
                p = select(g.sort_values(["d", "rule_rank"], kind="mergesort"), N_BASE, CAPITAL / N_BASE)
                res.append(p[["d", "code", "gap", "gap_true", "w", "net"]].assign(form=form, seed=s))
                print(f"{form} seed {s}", flush=True)
        pd.concat(res, ignore_index=True).to_parquet(OUT, index=False)
    report(pd.read_parquet(OUT))


if __name__ == "__main__":
    main()
