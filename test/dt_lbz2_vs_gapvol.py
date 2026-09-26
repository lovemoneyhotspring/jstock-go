"""LBZ2（bagging 5 本・順位の特徴量＋2 日続落・日ごとの z の教師データ）と gap_vol を、平常日で細かく比べる（記述、採否に使わない）。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記「bagging と 2 日続落」（2026-09-26 予備）。LBZ2 − G は平常日に
+3,547 円/日（t 1.50）。何を選んで、どこで勝ち負けしているかを分けて見る。学習は test/dt_lgbm_bag.py の BZ2 と同じ。

  PYTHONPATH=test test/.venv/bin/python test/dt_lbz2_vs_gapvol.py [--seeds 10]

誤差は予備と同じ slot 0859・9/24 まで（day_boot）。損益・銘柄の中身は誤差ありのシードを通して集計し、誤差なしも並べる。
"""

import argparse
import os

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_bag import MODELS, feats, fit, with_two_day  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, PRELIM_UNTIL, SPLIT  # noqa: E402
from dt_nscale import FEE, alloc_rule, calib_kappa, day_rules, max_dd  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

VS = ("G", "Z2")
TRAITS = [("gap_true", "始値のギャップ %", 100), ("key_sort", "gap_vol（見える）", 1), ("vol20", "vol20 %", 100),
          ("turnover_med", "売買代金 億", 1e-8), ("price", "株価 円", 1),
          ("ret5", "5 日騰落 %", 100), ("ret20", "20 日騰落 %", 100), ("pos20", "20 日レンジの位置", 1),
          ("A", "2 日続落の割合 %", 100), ("g_rank", "gap_vol での順位", 1)]


def picks(by, rules, uslow, kappa):
    """並べ方ごとに建てた銘柄（d, code, 建玉 w, 損益）を返す。小幅高の日は寄指 −1.5%。"""
    rows = []
    for v, x in by.items():
        for d, y in x.groupby("d"):
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
            if d in uslow:
                ok = y.loc[idx, "gap_true"].values <= LIMIT
                idx, w = idx[ok], w[ok]
            if not len(idx):
                continue
            s = y.loc[idx].assign(v=v, w=w)
            s["pnl"] = w * (s["y_raw"] - FEE - kappa * np.sqrt(w / s["turnover_med"]))
            rows.append(s)
    return pd.concat(rows)


def day_stats(p, days):
    x = p.groupby("d")["pnl"].sum().reindex(days).fillna(0.0)
    sd = x.std()
    return {"平均": x.mean(), "中央値": x.median(), "標準偏差": sd, "Sharpe（年率）": x.mean() / sd * np.sqrt(245) if sd else np.nan,
            "勝ち日 %": (x > 0).mean() * 100, "最悪日": x.min(), "最良日": x.max(), "最大DD": max_dd(x.values),
            "年率 %": x.mean() * 245 / CAP * 100}


def dd_by_year(x):
    """年ごとの最大 DD（その年の中の累積の山から谷）。"""
    return {y: max_dd(x[x.index.year == y].values) for y in sorted(set(x.index.year))}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    a = ap.parse_args()
    os.environ["DT_ERR_UNTIL"] = PRELIM_UNTIL

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
    fold_of = np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right")
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    lowm = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    normal = alld[~lowm]
    uslow = set(alld[lowm])
    shock = set(rules.index[rules["mult"] > 1])
    mkt = te.groupby("d")["y_raw"].mean()      # その日の候補全体の寄→引（地合い）

    def run(g):
        gf = pd.Series(fold_of, index=te.index).loc[g.index].values
        s = np.empty(len(g))
        for k, m in enumerate(models):
            s[gf == k] = m.predict(feats(g[gf == k], kind))
        gg = cap_and_rank(g, -g["key_sort"].values)
        g = g.assign(g_rank=g.index.map(gg["rank"]))
        by = {"G": gg.assign(g_rank=gg["rank"]), "Z2": cap_and_rank(g, s)}
        return picks(by, rules, uslow, calib_kappa(by["G"], I0))

    runs = {"誤差なし": run(sim.seen(te, np.zeros(len(te))))}
    pools = sim.error_pools("0859", "2026-09-11", mode="day_boot")
    ps = []
    for seed in range(a.seeds):
        ps.append(run(sim.seen(te, sim.draw_errors(pools, te, seed))).assign(seed=seed))
        print(f"seed {seed}", flush=True)
    err = pd.concat(ps)

    def per_seed(p, f):
        """シードごとに f を測って平均する。"""
        return pd.DataFrame([f(q) for _, q in p.groupby("seed")]).mean()

    pn = lambda p: p[p["d"].isin(normal)]  # noqa: E731
    print(f"\n平常日 {len(normal)} 日（2019-09-17〜2026-09-15、12 月と米国小幅高の日を除く）、1,000 万・10 位まで、"
          f"誤差ありは {a.seeds} シードの平均（円/日）")

    print("\n## 1. 日次の損益")
    tab = {}
    for v in VS:
        tab[f"{v}（誤差あり）"] = per_seed(pn(err[err["v"] == v]), lambda q: day_stats(q, normal))
        tab[f"{v}（誤差なし）"] = pd.Series(day_stats(pn(runs["誤差なし"][runs["誤差なし"]["v"] == v]), normal))
    print(pd.DataFrame(tab).round(1).to_string())

    daily = {v: pd.concat([q.groupby("d")["pnl"].sum().reindex(normal).fillna(0.0) for _, q in pn(err[err["v"] == v]).groupby("seed")],
                          axis=1).mean(axis=1) for v in VS}
    diff = daily["Z2"] - daily["G"]
    print(f"\n日次の損益の相関 {daily['Z2'].corr(daily['G']):.2f}、Z2 が勝った日 {(diff > 0).mean() * 100:.1f}%、"
          f"差の平均 {diff.mean():+,.0f}・中央値 {diff.median():+,.0f}")

    print("\n## 2. 年ごと・日の区分ごとの差（Z2 − G、誤差あり）")
    yr = normal.year
    print("  年: " + " ".join(f"{y}:{diff[yr == y].mean():+,.0f}（G {daily['G'][yr == y].mean():+,.0f}）" for y in sorted(set(yr))))
    sh = normal.isin(list(shock))
    print(f"  ショック日 {sh.sum()} 日: {diff[sh].mean():+,.0f}（G {daily['G'][sh].mean():+,.0f}）、"
          f"それ以外 {(~sh).sum()} 日: {diff[~sh].mean():+,.0f}（G {daily['G'][~sh].mean():+,.0f}）")
    m = mkt.reindex(normal)
    q = pd.qcut(m, 5, labels=["最弱", "弱", "中", "強", "最強"])
    print("  地合い（候補全体の寄→引の 5 分位）: " + " ".join(
        f"{lab} {diff[q == lab].mean():+,.0f}（G {daily['G'][q == lab].mean():+,.0f}）" for lab in q.cat.categories))
    print(f"  前半（〜2022）{diff[normal < SPLIT].mean():+,.0f}・後半（2023〜）{diff[normal >= SPLIT].mean():+,.0f}")

    print("\n## 3. 選ぶ銘柄（平常日、誤差あり）")
    e = pn(err)
    cnt = e.groupby(["seed", "v", "d"]).size().groupby("v").mean()
    amt = e.groupby(["seed", "v", "d"])["w"].sum().groupby("v").mean()
    both = e.groupby(["seed", "d", "code"])["v"].nunique()
    share = (both == 2).groupby(level=[0, 1]).sum() / e[e["v"] == "G"].groupby(["seed", "d"]).size()
    print(f"  1 日の本数 G {cnt['G']:.2f}・Z2 {cnt['Z2']:.2f}、建玉の合計 G {amt['G'] / 1e4:,.0f} 万・Z2 {amt['Z2'] / 1e4:,.0f} 万、"
          f"G の銘柄のうち Z2 も選ぶ割合 {share.mean() * 100:.1f}%")
    rows = {}
    for v in VS:
        x = e[e["v"] == v]
        rows[v] = {lab: (x[c].astype(float).replace([np.inf, -np.inf], np.nan) * sc).mean() for c, lab, sc in TRAITS}
        rows[v]["中央値の売買代金 億"] = x["turnover_med"].median() / 1e8
    print(pd.DataFrame(rows).round(2).to_string())
    print("  業種（建てた本数の割合 %、差の大きい順に 6）:")
    sec = e.groupby("v")["sector"].value_counts(normalize=True).unstack(0).fillna(0) * 100
    sec["差"] = sec["Z2"] - sec["G"]
    print(sec.reindex(sec["差"].abs().sort_values(ascending=False).index).head(6).round(1).to_string())

    print("\n## 4. 1 本あたり（平常日、誤差あり）")
    e = e.assign(both=e.set_index(["seed", "d", "code"]).index.map(both) == 2)
    for v in VS:
        x = e[e["v"] == v]
        print(f"  {v}: 寄→引 {x['y_raw'].mean() * 1e4:+.1f} bp・勝率 {(x['y_raw'] > 0).mean() * 100:.1f}%・"
              f"1 本の損益 {x['pnl'].mean():+,.0f} 円")
    for lab, x in [("両方が選ぶ", e[(e["v"] == "G") & e["both"]]), ("G だけ", e[(e["v"] == "G") & ~e["both"]]),
                   ("Z2 だけ", e[(e["v"] == "Z2") & ~e["both"]])]:
        print(f"  {lab}: 1 日 {x.groupby(['seed', 'd']).size().sum() / a.seeds / len(normal):.2f} 本、寄→引 "
              f"{x['y_raw'].mean() * 1e4:+.1f} bp・勝率 {(x['y_raw'] > 0).mean() * 100:.1f}%、ギャップ {x['gap_true'].mean() * 100:.2f}%・"
              f"2 日続落 {x['A'].mean() * 100:.0f}%・売買代金の中央値 {x['turnover_med'].median() / 1e8:.1f} 億")
    print("  見える順位（gap_vol）の帯ごとの Z2 の本数と寄→引:")
    z = e[e["v"] == "Z2"]
    band = pd.cut(z["g_rank"], [0, 3, 10, 20, 40, 1e9], labels=["1-3", "4-10", "11-20", "21-40", "41+"])
    print("   " + " ".join(f"{b} {(band == b).sum() / a.seeds / len(normal):.2f} 本/日・{z.loc[band == b, 'y_raw'].mean() * 1e4:+.0f} bp"
                           for b in band.cat.categories))

    print("\n## 5. 年ごとの最大 DD（平常日の累積、誤差あり、万円）")
    for v in VS:
        print(f"  {v}: " + " ".join(f"{y}:{dd / 1e4:,.0f}" for y, dd in dd_by_year(daily[v]).items()))
    print("  最悪の 10 日（Z2 の損益の小さい順）:")
    for d in daily["Z2"].nsmallest(10).index:
        print(f"    {d:%Y-%m-%d}: Z2 {daily['Z2'][d]:+,.0f}・G {daily['G'][d]:+,.0f}・地合い {mkt[d] * 1e4:+.0f} bp"
              + ("・ショック日" if d in shock else ""))


if __name__ == "__main__":
    main()
