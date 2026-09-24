"""daytrade ロング: 米国小幅高の日の本番の形（LightGBM × 寄指 −1.5% × 規則 R÷7）を、休む（= 0）と比べる。

規則 R の研究（test/dt_nscale_ranker.py）は小幅高の日を休みとして外し、小幅高の日の寄指（test/dt_limit_uslow.py）は
上位 3〜12 本の等額（500 万 ÷ 3）で測った。2026-09-25 から本番はこの日も規則 R で配分するので、組み合わせを測る。

根拠・事前登録: vault 20-research/2026-09-jp-daytrade-nscale.md「事前登録（小幅高の日の R÷7、2026-09-25）」

  PYTHONPATH=test test/.venv/bin/python test/dt_nscale_uslow.py [--seeds 10]

事前登録（結果を見る前に記入）:
- 日: mb_panel で S&P500 前日比 0〜+1%・VIX ≤ 24（本番と同じ定義、test/dt_limit_uslow.us_low_days）。2019-09-17〜2026-09-15、12 月を除く
- 本命の形 P: LightGBM 順（業種上限後）に R÷7（1 銘柄 = min(売買代金 × 0.2%, 資金 ÷ 7)、10 位まで = max_positions）を割り当て、
  始値のギャップ（真値）< −1.5% の銘柄だけ約定。届かない枠は現金（後で埋めない）
- 誤差は slot 0859 の block（test/dt_limit_uslow.py の主）、10 シード。滑り I0 5 bp（test/dt_nscale.py の kappa）
- 判定: 資金 700 万と 1,500 万の両方で、P の小幅高の日の損益 > 0（シード平均の日次で t ≥ 2）、かつ未見（〜2024-09-01）・既見で符号が揃う
  → 本番の形のまま。満たさなければ us_skip_legs = "all" に戻す候補としてユーザに諮る
- 探索（判定に使わない）: 同じ形の gap_vol・20 位まで・上位 3 本の逆ボラ（前の検証の形）、流動性別コストの上乗せ
"""

import argparse

import duckdb
import numpy as np
import pandas as pd

from dt_lgbm_train import ranked, raw_features
from dt_limit_uslow import us_low_days
from dt_nscale import alloc_fixed_iv, alloc_rule, calib_kappa, max_dd, pnl_day, tstat
from dt_nscale_ranker import ranked_by
from dt_preopen_sim import BANDS, ERR_SQL, seen
from dt_rank_compare import CAND, EMBARGO, LAST_DAY, SEEN_FROM, draw, fit
from dt_wf_target import FOLD_STARTS, liq_cost_bp

CAPS = [7e6, 1.5e7]
LIMIT = -0.015  # 本番の preopen_limit_pct_us_low = 1.5
FORMS = {
    "R÷7・10 本": lambda x, c: alloc_rule(x, 0.002, 10, c, k=7),
    "R÷7・20 本": lambda x, c: alloc_rule(x, 0.002, 20, c, k=7),
    "上位 3 本・逆ボラ": lambda x, c: alloc_fixed_iv(x, 3, c),
}
PRIMARY = ("R÷7・10 本", "lgbm")


def filled_pnl(x, idx, w, kappa):
    """寄指: 始値のギャップ（真値）が指値より下の銘柄だけ始値で約定する。返り値は（損益, 約定した建玉, 約定本数）。"""
    m = x.loc[idx, "gap_true"].values < LIMIT
    if not m.any():
        return 0.0, 0.0, 0
    return pnl_day(x, idx[m], w[m], kappa), float(w[m].sum()), int(m.sum())


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    a = ap.parse_args()
    err = duckdb.sql(ERR_SQL, params=["0859", "2026-09-11", "2026-09-11"]).df()
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
    print(f"学習 {len(models)} 本（木 {[m.n_estimators for m in models]}）", flush=True)
    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    day_idx = te["d"].rank(method="dense").astype(int).values - 1
    low = te["d"].isin(us_low_days()).values  # 誤差は全日で引いてから小幅高の日だけ残す（dt_limit_uslow と同じ乱数の並び）
    te_low = te[low]
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te_low["d"].values, side="right"),
                        index=te_low.index)
    lowd = pd.DatetimeIndex(sorted(te_low["d"].unique()))
    n_all = te["d"].nunique()
    acc, stat = {}, {}
    for s in range(a.seeds):
        e = draw(np.random.default_rng(s), err, band, day_idx, "block")
        g = seen(te_low, e[low])
        X = ranked(raw_features(g), g["d"]).values
        gf = fold_of.loc[g.index].values
        score = np.empty(len(g))
        for k, model in enumerate(models):
            score[gf == k] = model.predict(X[gf == k])
        kappa = calib_kappa(ranked_by(g, -g["key_sort"].values), 5e-4)
        for cost in ("I0 5bp", "流動性別"):
            gc = g if cost == "I0 5bp" else g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
            for ranker, sc in (("lgbm", score), ("gap_vol", -gc["key_sort"].values)):
                r = ranked_by(gc, sc)
                for C in CAPS:
                    for form, fn in FORMS.items():
                        out = {d: filled_pnl(x, *fn(x, C), kappa) for d, x in r.groupby("d")}
                        v = pd.Series({d: o[0] for d, o in out.items()}).reindex(lowd).fillna(0.0)
                        acc.setdefault((cost, C, form, ranker), []).append(v)
                        stat.setdefault((cost, C, form, ranker), []).append(
                            pd.DataFrame([(o[1] / C, o[2]) for o in out.values()], columns=["expo", "n"]))
        print(f"seed {s}", flush=True)

    print(f"\n小幅高の日 {len(lowd)} 日 / 全 {n_all} 日（{lowd[0]:%Y-%m-%d}〜{lowd[-1]:%Y-%m-%d}、12 月を除く）。"
          f"未見 〜{SEEN_FROM:%Y-%m-%d} / 既見 以降。比べる相手は休む = 0")
    print("bp は資金に対する小幅高の日 1 日あたり、万円/年は全日に均した年額（245 日）")
    for cost in ("I0 5bp", "流動性別"):
        for C in CAPS:
            print(f"\n## {cost}・資金 {C / 1e4:.0f} 万")
            print("| 形 | 並べ方 | bp/日 | t | 未見 / 既見（bp） | 正のシード | 万円/年 | 平均の約定本数 | 約定 0 本の日 | 平均の建玉 | 最悪の日（bp） | 最大 DD（万円） |")
            print("|---|---|---|---|---|---|---|---|---|---|---|---|")
            for form in FORMS:
                for ranker in ("lgbm", "gap_vol"):
                    per = acc[(cost, C, form, ranker)]
                    x = sum(per) / len(per)
                    st = pd.concat(stat[(cost, C, form, ranker)])
                    un = lowd < SEEN_FROM
                    mark = " **（判定）**" if (form, ranker) == PRIMARY and cost == "I0 5bp" else ""
                    bp = lambda v: v.mean() / C * 1e4
                    print(f"| {form}{mark} | {ranker} | {bp(x):+.2f} | {tstat(x):.2f} | {bp(x[un]):+.2f} / {bp(x[~un]):+.2f} |"
                          f" {sum(p.mean() > 0 for p in per)}/{len(per)} | {x.sum() / n_all * 245 / 1e4:+.1f} |"
                          f" {st['n'].mean():.2f} | {(st['n'] == 0).mean() * 100:.0f}% | {st['expo'].mean() * 100:.1f}% |"
                          f" {min(p.min() for p in per) / C * 1e4:+.0f} | {np.mean([max_dd(p.values) for p in per]) / 1e4:.1f} |")


if __name__ == "__main__":
    main()
