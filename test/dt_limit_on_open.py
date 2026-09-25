"""寄る前のロングを寄指（寄付条件つきの指値）に替えたら: 気配の誤差を入れた模擬。

根拠: vault 20-research/2026-09-jp-daytrade-limit-on-open.md（事前登録 2026-09-21）

  test/.venv/bin/python test/dt_limit_on_open.py [--seeds 20] [--side-seeds 10] [--slot 085930] [--err-since 2026-09-11]
  test/.venv/bin/python test/dt_limit_on_open.py --report-only [--detail b0_m0_k3_cap,b0.4_m1_k6_avg]

土台は test/dt_rank_compare.py と同じ（候補表・誤差の引き方・米国小幅高の日は休む＝G0）。並べ方は gap_vol だけで、学習は無い。
  B      上位 3 本を寄成（今の本番）。全部が始値で建つ
  寄指   上位 K 本に指値のギャップ L = min(β × 見えるギャップ + m, 0) を置き、始値のギャップ < L の銘柄だけ約定。
         約定しない枠は現金のまま。cap は K 本の重みの合計 = 1、avg は 1 注文を今と同じ大きさ（合計 K/3）にする
判定は P1〜P4（β 0.6、m 0 / 1%、K 3 / 6、cap）× 誤差の形 block x1.0。格子の残りは探索として記録するだけ。
"""

import argparse
import itertools

import duckdb
import numpy as np
import pandas as pd

from dt_preopen_sim import SNAP_SLOT, error_frame
from dt_lgbm_train import VOL_FLOOR
from dt_preopen_sim import BANDS, ERR_SQL, seen
from dt_rank_compare import CAND, LAST_DAY, SEEN_FROM, US, draw, tstat
from dt_wf_target import FOLD_STARTS, liq_cost_bp

CAPITAL = 5_000_000  # 本番のロングの max_capital。B は 3 本で割る
N_BASE = 3
BETAS = (1.0, 0.8, 0.6, 0.4, 0.0)
MARGINS = (0.0, 0.005, 0.01, 0.02)
KS = (3, 6, 9)
PRIMARY = {"P1": (0.6, 0.0, 3, "cap"), "P2": (0.6, 0.01, 3, "cap"), "P3": (0.6, 0.0, 6, "cap"), "P4": (0.6, 0.01, 6, "cap")}
SEEN_BANDS = [-np.inf, -0.05, -0.03, -0.01, 0.0]
MEASURED = (3.8, 17.2, 22.9, 31.5)  # test/dt_limit_fill_rate.py（8:59:08、6 営業日）
OUT = "test/out/dt_limit_on_open"


def select(g, k, budget):
    """gap_vol 順に K 本。1 単元が 1 注文の予算を超える銘柄は飛ばし、業種は 1 銘柄まで（dt_wf_target.pick と同じ規則）。"""
    idx = []
    for _, day in g[["d", "price", "sector"]].groupby("d", sort=False):
        out, sectors = [], set()
        for r in day.head(60).itertuples():
            if r.price * 100 > budget or (r.sector and r.sector in sectors):
                continue
            if r.sector:
                sectors.add(r.sector)
            out.append(r.Index)
            if len(out) == k:
                break
        idx += out
    p = g.loc[idx, ["d", "code", "gap", "gap_true", "y_raw", "vol20", "turnover_med"]].copy()
    inv = 1.0 / np.maximum(p["vol20"].fillna(VOL_FLOOR), VOL_FLOOR)
    p["w"] = inv / inv.groupby(p["d"]).transform("sum")
    p["net"] = p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4
    return p


def simulate(te, e, form, seed):
    g = seen(te, e).sort_values(["d", "rule_rank"], kind="mergesort")
    daily, picks = [], []
    # 模擬の点検: 指値 = 生の気配（β 1・m 0）の約定率を、見えるギャップの帯ごとに
    chk = pd.DataFrame({"band": np.digitize(g["gap"].values, SEEN_BANDS[1:-1], right=False), "fill": (g["gap_true"] < g["gap"]).values})
    chk = chk.groupby("band")["fill"].agg(["sum", "size"]).reset_index().assign(form=form, seed=seed)

    base = select(g, N_BASE, CAPITAL / N_BASE)
    daily.append(base.assign(ret=base["w"] * base["net"]).groupby("d").agg(ret=("ret", "sum")).reset_index()
                 .assign(config="B", n_fill=N_BASE, expo=1.0))
    for k, sizing in itertools.product(KS, ("cap", "avg")):
        if k == N_BASE and sizing == "avg":
            continue  # 3 本では cap と同じ
        p = base if k == N_BASE else select(g, k, CAPITAL / k if sizing == "cap" else CAPITAL / N_BASE)
        scale = 1.0 if sizing == "cap" else k / N_BASE
        for beta, m in itertools.product(BETAS, MARGINS):
            fill = (p["gap_true"] < np.minimum(beta * p["gap"] + m, 0.0)).values
            w = p["w"].values * scale * fill
            name = f"b{beta:g}_m{m * 100:g}_k{k}_{sizing}"
            d = pd.DataFrame({"d": p["d"].values, "ret": w * p["net"].values, "n_fill": fill.astype(int), "expo": w})
            daily.append(d.groupby("d").sum().reset_index().assign(config=name))
            if (beta, m, k, sizing) in PRIMARY.values():
                picks.append(p[["d", "code", "gap", "gap_true", "net"]].assign(filled=fill, config=name))
    tag = dict(form=form, seed=seed)
    return pd.concat(daily, ignore_index=True).assign(**tag), pd.concat(picks, ignore_index=True).assign(**tag), chk


def stats(x):
    cum = x.cumsum()
    return x.mean() * 1e4, tstat(x), x.mean() / x.std(ddof=1) * np.sqrt(245), (cum.cummax() - cum).max() * 100


def report(r, picks, chk, all_days, detail=()):
    us = pd.read_parquet(US)[["date", "spx_ret1", "vix"]].rename(columns={"date": "d"})
    us["us_low"] = (us["spx_ret1"] >= 0) & (us["spx_ret1"] < 0.01) & (us["vix"].isna() | (us["vix"] <= 24))
    us_low = us.set_index("d")["us_low"].reindex(all_days).fillna(False).astype(bool).values
    trade_days = all_days[~us_low]
    unseen = all_days < SEEN_FROM

    def series(form, config, seed=None, col="ret"):
        q = r[(r["form"] == form) & (r["config"] == config)]
        if seed is not None:
            q = q[q["seed"] == seed]
        return q.groupby("d")[col].mean().reindex(all_days).fillna(0.0).where(~us_low, 0.0)

    print(f"\n{all_days[0]:%Y-%m-%d}〜{all_days[-1]:%Y-%m-%d}、{len(all_days)} 日（12 月を除く）、うち米国小幅高で休む {us_low.sum()} 日、流動性別コスト")

    print("\n## 模擬の点検: 指値 = 生の気配（β 1・m 0）の約定率（%）、見えるギャップの帯ごと")
    c = chk[chk["form"] == "block"].groupby("band")[["sum", "size"]].sum()
    print("| 帯 | 模擬 | 実測 |\n|---|---|---|")
    for b, label in enumerate(("−5% 以下", "−5〜−3%", "−3〜−1%", "−1〜0%")):
        print(f"| {label} | {c.loc[b, 'sum'] / c.loc[b, 'size'] * 100:.1f} | {MEASURED[b]} |")

    names = {v: k for k, v in PRIMARY.items()}
    for form in ("block", "block_x0.8", "iid"):
        seeds = sorted(r.loc[r["form"] == form, "seed"].unique())
        b = series(form, "B")
        print(f"\n## 誤差の形 {form}（シード {len(seeds)} 本）。B（寄成・3 本）= {stats(b)[0]:+.2f} bp/日、t {stats(b)[1]:.2f}、"
              f"Sharpe {stats(b)[2]:.2f}、最大 DD {stats(b)[3]:.1f}%")
        print("| 設定 | bp/日 | Sharpe | 最大 DD | B との差 | t | 未見 / 既見の差 | 差 > 0 のシード | 約定率 | 平均の建玉 |")
        print("|---|---|---|---|---|---|---|---|---|---|")
        for (beta, m, k, sizing), pn in names.items():
            cfg = f"b{beta:g}_m{m * 100:g}_k{k}_{sizing}"
            x, d = series(form, cfg), series(form, cfg) - b
            pos = sum((series(form, cfg, s) - series(form, "B", s)).mean() > 0 for s in seeds)
            q = r[(r["form"] == form) & (r["config"] == cfg) & r["d"].isin(trade_days)]
            f = picks[(picks["form"] == form) & (picks["config"] == cfg) & picks["d"].isin(trade_days)]["filled"]
            print(f"| {pn}（β {beta:g}・m {m * 100:g}%・K {k}） | {stats(x)[0]:+.2f} | {stats(x)[2]:.2f} | {stats(x)[3]:.1f}% | {d.mean() * 1e4:+.2f} |"
                  f" {tstat(d):.2f} | {d[unseen].mean() * 1e4:+.2f} / {d[~unseen].mean() * 1e4:+.2f} | {pos}/{len(seeds)} |"
                  f" {f.mean() * 100:.0f}% | {q['expo'].mean():.2f} |")

    if detail:
        print("\n## 探索の設定の内訳（判定に使わない）: block")
        print("| 設定 | bp/日 | Sharpe | 最大 DD | B との差 | t | 未見 / 既見の差 | 差 > 0 のシード | 1 日の約定本数 | 平均 / 最大の建玉 | 最悪の日 |")
        print("|---|---|---|---|---|---|---|---|---|---|---|")
        seeds = sorted(r.loc[r["form"] == "block", "seed"].unique())
        b = series("block", "B")
        for cfg in detail:
            x, d = series("block", cfg), series("block", cfg) - b
            pos = sum((series("block", cfg, s) - series("block", "B", s)).mean() > 0 for s in seeds)
            q = r[(r["form"] == "block") & (r["config"] == cfg) & r["d"].isin(trade_days)]
            print(f"| {cfg} | {stats(x)[0]:+.2f} | {stats(x)[2]:.2f} | {stats(x)[3]:.1f}% | {d.mean() * 1e4:+.2f} | {tstat(d):.2f} |"
                  f" {d[unseen].mean() * 1e4:+.2f} / {d[~unseen].mean() * 1e4:+.2f} | {pos}/{len(seeds)} | {q['n_fill'].mean():.2f} |"
                  f" {q['expo'].mean():.2f} / {q['expo'].max():.2f} | {q['ret'].min() * 1e4:+.0f} bp |")

    print("\n## 逆選択（基準 6）: 選んだ銘柄の始値→引け（コスト後、bp）。block、休む日を除く")
    print("| 設定 | 約定した側 | 約定しなかった側 | 約定した側の実際のギャップ −5% 以下 | 1 日の約定 0 本 / 全部 |")
    print("|---|---|---|---|---|")
    pk = picks[(picks["form"] == "block") & picks["d"].isin(trade_days)]
    for (beta, m, k, sizing), pn in names.items():
        cfg = f"b{beta:g}_m{m * 100:g}_k{k}_{sizing}"
        q = pk[pk["config"] == cfg]
        nf = q.groupby(["seed", "d"])["filled"].agg(["sum", "size"])
        print(f"| {pn} | {q.loc[q['filled'], 'net'].mean() * 1e4:+.1f}（{q['filled'].sum():,} 行） | {q.loc[~q['filled'], 'net'].mean() * 1e4:+.1f}"
              f"（{(~q['filled']).sum():,} 行） | {(q.loc[q['filled'], 'gap_true'] <= -0.05).mean() * 100:.1f}% |"
              f" {(nf['sum'] == 0).mean() * 100:.0f}% / {(nf['sum'] == nf['size']).mean() * 100:.0f}% |")

    print("\n## 探索（判定に使わない）: block、B との差 bp/日（t）。行 β、列 m")
    for k, sizing in ((3, "cap"), (6, "cap"), (9, "cap"), (6, "avg"), (9, "avg")):
        print(f"\nK {k}・{sizing}" + ("（最大の建玉 %.1f 倍）" % (k / N_BASE) if sizing == "avg" else ""))
        print("| β | " + " | ".join(f"m {m * 100:g}%" for m in MARGINS) + " |\n|---|" + "---|" * len(MARGINS))
        b = series("block", "B")
        for beta in BETAS:
            cells = [series("block", f"b{beta:g}_m{m * 100:g}_k{k}_{sizing}") - b for m in MARGINS]
            print(f"| {beta:g} | " + " | ".join(f"{d.mean() * 1e4:+.2f}（{tstat(d):.2f}）" for d in cells) + " |")
    worst = r[(r["form"] == "block") & r["config"].str.endswith("_avg")].groupby("config").agg(expo_max=("expo", "max"), ret_min=("ret", "min"))
    print(f"\navg の最大の建玉 {worst['expo_max'].max():.2f} 倍、最悪の日 {worst['ret_min'].min() * 1e4:+.0f} bp（総予算比）")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--side-seeds", type=int, default=10)
    ap.add_argument("--slot", default=SNAP_SLOT)
    ap.add_argument("--err-since", default="2026-09-11")
    ap.add_argument("--report-only", action="store_true")
    ap.add_argument("--detail", default="", help="探索の設定の内訳を出す（例: b0_m0_k3_cap,b0.4_m1_k6_avg）")
    a = ap.parse_args()

    if not a.report_only:
        err = error_frame(a.slot, a.err_since)
        print(f"誤差の実測: slot {a.slot}、{err['d'].nunique()} 日、{len(err):,} 行", flush=True)
        err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
        df = pd.read_parquet(CAND)
        df = df[df["d"] <= LAST_DAY].copy()
        df["price"] = df["o"]
        df["sector"] = df["sector"].fillna("")
        te = df[(df["d"] >= FOLD_STARTS[0]) & (df["d"].dt.month != 12)].copy()
        band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
        day_idx = te["d"].rank(method="dense").astype(int).values - 1
        res = []
        for form, scale, mode, n in (("block", 1.0, "block", a.seeds), ("block_x0.8", 0.8, "block", a.side_seeds), ("iid", 1.0, "iid", a.side_seeds)):
            for s in range(n):
                res.append(simulate(te, draw(np.random.default_rng(s), err, band, day_idx, mode) * scale, form, s))
                print(f"{form} seed {s}", flush=True)
        for i, name in enumerate(("daily", "picks", "check")):
            pd.concat([x[i] for x in res], ignore_index=True).to_parquet(f"{OUT}_{name}.parquet", index=False)
        pd.DataFrame({"d": sorted(te["d"].unique())}).to_parquet(f"{OUT}_days.parquet", index=False)

    r, picks, chk = (pd.read_parquet(f"{OUT}_{n}.parquet") for n in ("daily", "picks", "check"))
    report(r, picks, chk, pd.DatetimeIndex(pd.read_parquet(f"{OUT}_days.parquet")["d"]), [c for c in a.detail.split(",") if c])


if __name__ == "__main__":
    main()
