"""寄る前の発注の形で、並べ方（LightGBM / gap_vol / 併用）を対応のある形で比べる。

根拠・事前登録: vault 20-research/2026-09-jp-daytrade-rankby-preopen-fair.md

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  test/.venv/bin/python test/dt_rank_compare.py [--seeds 20] [--side-seeds 10] [--slot snap] [--err-since 2026-09-11]

誤差の入れ方・候補の作り直しは test/dt_preopen_sim.py と同じ。違いは
  - 学習が walk-forward（1 年ずつ 7 本。dt_preopen_sim は 2024-09 で 1 分割）
  - 並べ方 3 本（lgbm / gap_vol / 素の gap）を同じ誤差の乱数で回し、日次の損益から設定 L・G・G0・H を組む
  - 誤差の形を 4 つ回す: x1.0（判定）、x0.8、block（記録の 1 日をまるごと引く）、upper（誤差なし）
"""

import argparse

import duckdb
import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features
from dt_preopen_sim import BANDS, ERR_SQL, seen, with_rule_rank, SNAP_SLOT, error_frame
from dt_wf_target import EMBARGO, FOLD_STARTS, evaluate, liq_cost_bp

CAND = "test/out/dt_candidates_wide.parquet"
US = "test/out/mb_panel.parquet"
LAST_DAY = "2026-09-15"  # 米国の値（mb_panel）がある最後の日
SEEN_FROM = pd.Timestamp("2024-09-02")  # ここから先は事前登録の前に一度見ている
SLOT, ERR_SINCE = SNAP_SLOT, "2026-09-11"
OUT = "test/out/dt_rank_compare_daily.parquet"


def fit(tr):
    tr = with_rule_rank(tr)
    X = ranked(raw_features(tr), tr["d"]).values
    y = tr.groupby("d")["y_raw"].rank(pct=True).values
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    final = LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW)
    return final.fit(X, y)


def draw(rng, err, band, day_idx, mode):
    """帯ごとに誤差を引く。block は模擬の 1 日ごとに記録の 1 日を選び、その日の誤差だけから引く。"""
    e = np.zeros(len(band))
    rec_days = np.sort(err["d"].unique())
    if mode == "block":
        pick = rng.integers(len(rec_days), size=day_idx.max() + 1)[day_idx]
    for b in np.unique(band):
        pool_all = err.loc[err["band"] == b, "e"].values
        if mode != "block":
            e[band == b] = rng.choice(pool_all, size=int((band == b).sum()))
            continue
        for k, rd in enumerate(rec_days):
            m = (band == b) & (pick == k)
            pool = err.loc[(err["band"] == b) & (err["d"] == rd), "e"].values
            e[m] = rng.choice(pool if len(pool) >= 5 else pool_all, size=int(m.sum()))
    return e


def simulate(te, models, fold_of, e, form, seed):
    g = seen(te, e)
    score = np.empty(len(g))
    X = ranked(raw_features(g), g["d"]).values
    gf = fold_of.loc[g.index].values
    for k, model in enumerate(models):
        score[gf == k] = model.predict(X[gf == k])
    out = []
    for ranker, s in (("lgbm", score), ("gap_vol", -g["key_sort"].values), ("gap", -g["gap"].values)):
        p = evaluate(g, s, ranker, seed).merge(g[["d", "code", "gap_true"]], on=["d", "code"])
        p["ret"] = p["w"] * (p["y_raw"] - liq_cost_bp(p["turnover_med"].values) / 1e4)
        d = p.groupby("d").agg(ret=("ret", "sum"), n=("ret", "size"), n_pos=("gap_true", lambda x: (x >= 0).sum()),
                               n_deep=("gap_true", lambda x: (x <= -0.05).sum())).reset_index()
        out.append(d.assign(form=form, ranker=ranker, seed=seed))
    return pd.concat(out, ignore_index=True)


def tstat(x):
    return x.mean() / (x.std(ddof=1) / np.sqrt(len(x)))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--side-seeds", type=int, default=10, help="x0.8 と block のシード数（判定の x1.0 は --seeds）")
    ap.add_argument("--slot", default=SLOT, help="誤差を測る板の時刻。8:59:45 の記録が 20 営業日溜まったら 085945")
    ap.add_argument("--err-since", default=ERR_SINCE)
    ap.add_argument("--report-only", action="store_true", help="保存済みの日次の結果から集計だけやり直す")
    a = ap.parse_args()

    if a.report_only:
        r = pd.read_parquet(OUT)
        report(r, pd.DatetimeIndex(sorted(r.loc[r["form"] == "upper", "d"].unique())))
        return
    err = error_frame(a.slot, a.err_since)
    print(f"誤差の実測: slot {a.slot}、{err['d'].nunique()} 日、{len(err):,} 行", flush=True)
    err["band"] = np.digitize(err["g"], BANDS[1:-1], right=True)
    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]

    models = []
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)]))
        print(f"{FOLD_STARTS[k]}: 木 {models[-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    day_idx = te["d"].rank(method="dense").astype(int).values - 1

    res = [simulate(te, models, fold_of, np.zeros(len(te)), "upper", 0)]
    for form, scale, mode, n in (("x1.0", 1.0, "iid", a.seeds), ("x0.8", 0.8, "iid", a.side_seeds), ("block", 1.0, "block", a.side_seeds)):
        for s in range(n):
            e = draw(np.random.default_rng(s), err, band, day_idx, mode) * scale
            res.append(simulate(te, models, fold_of, e, form, s))
            print(f"{form} seed {s}", flush=True)
    r = pd.concat(res, ignore_index=True)
    r.to_parquet(OUT, index=False)
    report(r, pd.DatetimeIndex(sorted(te["d"].unique())))


def report(r, all_days):

    us = pd.read_parquet(US)[["date", "spx_ret1", "vix"]].rename(columns={"date": "d"})
    us["us_low"] = (us["spx_ret1"] >= 0) & (us["spx_ret1"] < 0.01) & (us["vix"].isna() | (us["vix"] <= 24))
    us_low = us.set_index("d")["us_low"].reindex(all_days).fillna(False).astype(bool).values

    def series(form, ranker, seed=None):
        q = r[(r["form"] == form) & (r["ranker"] == ranker)]
        if seed is not None:
            q = q[q["seed"] == seed]
        return q.groupby("d")["ret"].mean().reindex(all_days).fillna(0.0)

    def configs(form, seed=None):
        lg, gv, gp = series(form, "lgbm", seed), series(form, "gap_vol", seed), series(form, "gap", seed)
        return {"L（現行）": lg, "G": gv, "G0": gv.where(~us_low, 0.0), "H": lg.where(us_low, gv), "gap（物差し）": gp}

    print(f"\n{all_days[0]:%Y-%m-%d}〜{all_days[-1]:%Y-%m-%d}、{len(all_days)} 日（12 月を除く）、米国小幅高 {us_low.sum()} 日、流動性別コスト")
    for form in ("x1.0", "x0.8", "block", "upper"):
        c = configs(form)
        seeds = sorted(r.loc[r["form"] == form, "seed"].unique())
        print(f"\n## 誤差の形 {form}（シード {len(seeds)} 本）")
        print("| 設定 | bp/日 | t | Sharpe | 最大 DD | L との差 | t | 未見 / 既見の期間の差 | 差 > 0 のシード | 小幅高の日 / それ以外 |")
        print("|---|---|---|---|---|---|---|---|---|---|")
        for name, x in c.items():
            cum = x.cumsum()
            row = (f"| {name} | {x.mean() * 1e4:+.2f} | {tstat(x):.2f} | {x.mean() / x.std(ddof=1) * np.sqrt(250):.2f} |"
                   f" {(cum.cummax() - cum).max() * 100:.1f}% |")
            split = f" {x[us_low].mean() * 1e4:+.2f} / {x[~us_low].mean() * 1e4:+.2f} |"
            if name.startswith("L"):
                print(row + " — | — | — | — |" + split)
                continue
            d = x - c["L（現行）"]
            pos = sum((configs(form, s)[name] - configs(form, s)["L（現行）"]).mean() > 0 for s in seeds)
            print(row + f" {d.mean() * 1e4:+.2f} | {tstat(d):.2f} | {d[d.index < SEEN_FROM].mean() * 1e4:+.2f} / {d[d.index >= SEEN_FROM].mean() * 1e4:+.2f} |"
                  f" {pos}/{len(seeds)} |" + split)

    print("\n## 選定銘柄の実際の始値のギャップ（x1.0）")
    q = r[r["form"] == "x1.0"].groupby("ranker")[["n", "n_pos", "n_deep"]].sum()
    print((q[["n_pos", "n_deep"]].div(q["n"], axis=0) * 100).round(1).rename(columns={"n_pos": "実際は正 %", "n_deep": "−5% 以下 %"}).to_string())


if __name__ == "__main__":
    main()
