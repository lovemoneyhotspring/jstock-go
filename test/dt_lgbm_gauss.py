"""LightGBM の教師データの両端を重くする方向（正規分位点・順位との混合）と、特徴量の正規分位点化を試す。平常日の gap_vol との比較も同じ回で測る。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記「日ごとの標準得点」（2026-09-26 予備、test/dt_lgbm_zscore.py）。
教師データを正規分位点（LYG）にすると小幅高の日に唯一の正（+303、誤差なし +956）、平常日は z・正規分位点（LYZ・LYG）が
今の L より G を大きく上回った（+3,204・+2,679 対 +1,670）。ただし平常日の誤差なしでは L より小さい。
特徴量の正規分位点化は、ふつうの木では順位の単調な変換なので分岐が変わらない。葉に線形モデルを置く linear_tree なら
値の間隔が効くので、その形と組む（ユーザの提案）。

  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_gauss.py --prelim   # 予備（誤差なし＋9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_gauss.py --check    # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_gauss.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習・本番の形・気配の誤差は test/dt_lgbm_zscore.py と同じ（walk-forward 7 fold・L2 回帰・規則 R ÷7・10 位・1,000 万・
  小幅高の日は寄指 −1.5%・判定は 8:59:52 の層の材料 20 日を day_boot で 20 シード）。
  特徴量:  rank（今）、gauss = 日ごとの Φ⁻¹((順位 − 0.5) ÷ 欠けを除いた候補数)、欠けは 0
  教師データ: rank（今）、z（dt_lgbm_zscore と同じ ±3）、gauss（同）、
             mix = (一様の標準得点 (百分位 − 0.5)·√12 ＋ gauss) ÷ 2（順位と正規分位点の間。両端の重みを半分だけ足す）
  モデル（比べる形）:
    L    rank・rank（今）
    LYZ  rank・z        （dt_lgbm_zscore で登録済み。主 A では比べない）
    LYG  rank・gauss    （同）
    LYM  rank・mix
    LGG  gauss・gauss   （ふつうの木。LYG とほぼ同じになるはず＝確かめ）
    LGT  gauss・gauss、linear_tree=True（linear_lambda 1.0）
  判定 主 A（小幅高の日、− L）: LYM・LGG・LGT の 3 本。誤差込みの t ≥ 2.4 かつ差 > 0、誤差なしでも差 > 0、前半・後半とも差 > 0。
  判定 主 B（平常日＝小幅高でも 12 月でもない日、− G）: LYZ・LYG・LYM・LGG・LGT の 5 本。test/dt_rankby_normal_recheck.py の
    条件 1〜5 を、条件 1 の t を 2.6（5% ÷ 5）にして当てる（2. 差 > 0 のシードが 8 割以上、3. 前半・後半とも > 0、
    4. 誤差 0.8 倍・1.2 倍でも > 0、5. 流動性別コストを載せても > 0）。加えて 6. 平常日で L − G より大きい（今の LightGBM を超える）。
    すべて満たしたものを「平常日もこの形の LightGBM で並べる」候補とし、複数なら誤差込みの t の大きい方。
  記述（採否に使わない）: 木の本数、選んだ銘柄のギャップの深さ、年率と最大 DD、年ごと。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd
from lightgbm import LGBMRegressor, early_stopping, log_evaluation
from scipy.stats import norm

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_train import INNER_VALID_DAYS, KW, ranked, raw_features  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, NEED_DAYS, PRELIM_UNTIL, SPLIT, material_days  # noqa: E402
from dt_lgbm_zscore import Z_TARGET_CLIP, robust_z  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
from dt_wf_target import liq_cost_bp  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402
from dt_preopen_sim import with_rule_rank  # noqa: E402

T_A, T_B = 2.4, 2.6
SEEDS_OK = 0.8
SCALES = (0.8, 1.2)
LINEAR = dict(linear_tree=True, linear_lambda=1.0)
MODELS = {"M0": ("rank", "rank", {}), "MYZ": ("rank", "z", {}), "MYG": ("rank", "gauss", {}),
          "MYM": ("rank", "mix", {}), "MGG": ("gauss", "gauss", {}), "MGT": ("gauss", "gauss", LINEAR)}
VARIANTS = {"L": "M0", "LYZ": "MYZ", "LYG": "MYG", "LYM": "MYM", "LGG": "MGG", "LGT": "MGT"}
JUDGE_A = ("LYM", "LGG", "LGT")
JUDGE_B = ("LYZ", "LYG", "LYM", "LGG", "LGT")


def quantiles(x, days):
    """日ごとの (順位 − 0.5) ÷ 欠けを除いた数。欠けは NaN のまま。"""
    return (x.groupby(days).rank() - 0.5) / x.groupby(days).transform("count")


def feats(g, kind):
    raw = raw_features(g).replace([np.inf, -np.inf], np.nan)
    if kind == "rank":
        return ranked(raw, g["d"]).values
    return pd.DataFrame(norm.ppf(quantiles(raw, g["d"])), index=raw.index).fillna(0.0).values


def target(tr, kind):
    y, d = tr["y_raw"], tr["d"]
    if kind == "rank":
        return y.groupby(d).rank(pct=True).values
    if kind == "z":
        return robust_z(y, d, Z_TARGET_CLIP).where(y.notna()).values
    q = quantiles(y, d).values
    return norm.ppf(q) if kind == "gauss" else ((q - 0.5) * np.sqrt(12) + norm.ppf(q)) / 2


def fit(tr, kind, tkind, extra):
    tr = with_rule_rank(tr)
    X = feats(tr, kind)
    y = target(tr, tkind)
    days = np.array(sorted(tr["d"].unique()))
    core = (tr["d"] < days[len(days) - INNER_VALID_DAYS]).values
    m = LGBMRegressor(n_estimators=2000, **KW, **extra)
    m.fit(X[core], y[core], eval_set=[(X[~core], y[~core])], eval_metric="l2",
          callbacks=[early_stopping(50, verbose=False), log_evaluation(0)])
    return LGBMRegressor(n_estimators=m.best_iteration_ or 200, **KW, **extra).fit(X, y)


def ranked_all(g, models, gf):
    by = {"G": cap_and_rank(g, -g["key_sort"].values)}
    for v, name in VARIANTS.items():
        s = np.empty(len(g))
        for k, m in enumerate(models[name]):
            s[gf.values == k] = m.predict(feats(g[gf.values == k], MODELS[name][0]))
        by[v] = cap_and_rank(g, s)
    return by


def run_once(g, models, gf, rules, uslow, liq=False):
    if liq:
        g = g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
    by = ranked_all(g, models, gf)
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
        for name, (kind, tkind, extra) in MODELS.items():
            models[name].append(fit(tr, kind, tkind, extra))
        print(f"{FOLD_STARTS[k]}: 木 " + "・".join(f"{nm} {models[nm][-1].n_estimators}" for nm in MODELS) + " 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    lowm = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    normal = ~lowm
    uslow = set(alld[lowm])

    def series(g, liq=False):
        out, depth = run_once(g, models, fold_of.loc[g.index], rules, uslow, liq)
        return {v: s.reindex(alld).fillna(0.0) for v, s in out.items()}, depth

    upper, depth0 = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    names = ["G", *VARIANTS]
    acc = {sc: {v: [] for v in names} for sc in (1.0,) + SCALES}
    liqc = {v: [] for v in names}
    for seed in range(seeds):
        e = sim.draw_errors(pools, te, seed)
        for sc in acc:
            out, _ = series(sim.seen(te, e * sc))
            for v in names:
                acc[sc][v].append(out[v])
        out, _ = series(sim.seen(te, e), liq=True)
        for v in names:
            liqc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    avg = lambda xs: pd.concat(xs, axis=1).mean(axis=1)  # noqa: E731
    base = acc[1.0]
    mean = {v: avg(x) for v, x in base.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（米国小幅高の日 {lowm.sum()} 日・平常日 "
          f"{normal.sum()} 日）、資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{seeds} シード（円/日）")
    print(f"  小幅高の日の L の水準 {mean['L'][lowm].mean():+,.0f}（誤差なし {upper['L'][lowm].mean():+,.0f}）、"
          "選んだ銘柄の始値のギャップ（誤差なし、平均）" + "・".join(f"{v} {depth0[v] * 100:.2f}%" for v in names))

    print("\n主 A: 小幅高の日の − L")
    passed_a = []
    for v in [x for x in VARIANTS if x != "L"]:
        d = (mean[v] - mean["L"])[lowm]
        te_ = tstat_err(base[v], base["L"], lowm)
        up = (upper[v] - upper["L"])[lowm].mean()
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        ok = d.mean() > 0 and te_ >= T_A and up > 0 and first > 0 and second > 0
        mark = ("○" if ok else "×") if v in JUDGE_A else "（参考）"
        print(f"  {v} − L: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、誤差なし {up:+,.0f}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f} → {mark}")
        if ok and v in JUDGE_A:
            passed_a.append((te_, v))

    print("\n主 B: 平常日の − G")
    lq = {v: avg(x) for v, x in liqc.items()}
    ref = (mean["L"] - mean["G"])[normal].mean()
    passed_b = []
    for v in ["L", *JUDGE_B]:
        d = (mean[v] - mean["G"])[normal]
        te_ = tstat_err(base[v], base["G"], normal)
        pos = sum((x - g_)[normal].mean() > 0 for x, g_ in zip(base[v], base["G"]))
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        scaled = [(avg(acc[sc][v]) - avg(acc[sc]["G"]))[normal].mean() for sc in SCALES]
        d_lq = (lq[v] - lq["G"])[normal].mean()
        up = (upper[v] - upper["G"])[normal].mean()
        checks = [d.mean() > 0 and te_ >= T_B, pos >= SEEDS_OK * seeds, first > 0 and second > 0,
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
