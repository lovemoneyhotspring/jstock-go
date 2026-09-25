"""LightGBM の特徴量・教師データを日ごとの順位ではなく日ごとの標準得点（z）で渡し、日の中の間隔を残すと並びが良くなるかを見る。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記（2026-09-26、ユーザの提案）。順位は日の中の並びしか残さず、
「どれだけ離れているか」を捨てる。生の値は間隔を残すが日をまたいだ水準まで持ち込み、生の bp は日ごとの上下に埋もれた。
標準得点はその間に立つ（日の水準と幅は消し、日の中の間隔は残す）。ギャップは裾が厚く、平均と標準偏差では 1〜2 本の外れ値に
全体が引かれるので、中央値と MAD（×1.4826）で標準化し、端を刈る。

  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_zscore.py --prelim   # 予備（誤差なし＋9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_zscore.py --check    # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_lgbm_zscore.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習・本番の形・気配の誤差・判定の条件 2〜3 は test/dt_lgbm_target_bins.py と同じ（walk-forward 7 fold・L2 回帰・規則 R ÷7・
  10 位・1,000 万・小幅高の日は寄指 −1.5%・判定は 8:59:52 の層の材料 20 日を day_boot で 20 シード）。
  特徴量:
    rank  日ごとの百分位（今）
    z     日ごとの頑健な標準得点 (x − 中央値) ÷ (MAD × 1.4826) を ±5 で刈る。欠けと MAD 0（n_cand など日で一定の列）は 0
    rz    rank と z を並べる（32 列。並びと間隔の両方）
  教師データ:
    rank  寄→引の日ごとの百分位（今）
    z     寄→引の日ごとの頑健な標準得点を ±3 で刈る（L2 が外れ値に引かれないよう、特徴量より狭く刈る）
    gauss 日ごとの順位を正規分布の分位点に写す Φ⁻¹((順位 − 0.5) ÷ 候補数)。外れ値の影響は受けず、両端の差を順位より大きく扱う
  モデルと比べる形:
    L    特徴量 rank・教師データ rank（今）
    LZ   特徴量 z・教師データ rank     （特徴量の間隔だけ足す）
    LRZ  特徴量 rz・教師データ rank    （並びに間隔を足す）
    LYZ  特徴量 rank・教師データ z     （教師データの間隔だけ足す）
    LYG  特徴量 rank・教師データ gauss （教師データの両端を重くする）
    LZZ  特徴量 z・教師データ z        （提案そのもの）
  判定（主）: 米国小幅高の日の LZ・LRZ・LYZ・LYG・LZZ − L。5 本なので誤差込みの t ≥ 2.6（5% ÷ 5、両側）かつ差 > 0、
             誤差なしでも差 > 0、前半・後半とも差 > 0 をすべて満たしたものを「効く」。複数なら誤差込みの t の大きい方。
             どれも満たさなければ今のまま。
  記述（採否に使わない）: 平常日の − G、木の本数、選んだ銘柄のギャップの深さ、年率と最大 DD。
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
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402
from dt_preopen_sim import with_rule_rank  # noqa: E402

T_MIN = 2.6
Z_FEAT_CLIP, Z_TARGET_CLIP = 5.0, 3.0
MODELS = {"M0": ("rank", "rank"), "MZ": ("z", "rank"), "MRZ": ("rz", "rank"),
          "MYZ": ("rank", "z"), "MYG": ("rank", "gauss"), "MZZ": ("z", "z")}
VARIANTS = {"L": "M0", "LZ": "MZ", "LRZ": "MRZ", "LYZ": "MYZ", "LYG": "MYG", "LZZ": "MZZ"}


def robust_z(x, days, clip):
    """日ごとの (x − 中央値) ÷ (MAD × 1.4826) を ±clip で刈る。欠けと MAD 0 は 0。"""
    med = x.groupby(days).transform("median")
    mad = (x - med).abs().groupby(days).transform("median") * 1.4826
    z = (x - med) / mad.where(mad > 0)
    return z.clip(-clip, clip).fillna(0.0)


def feats(g, kind):
    raw = raw_features(g).replace([np.inf, -np.inf], np.nan)
    if kind == "rank":
        return ranked(raw, g["d"]).values
    z = robust_z(raw, g["d"], Z_FEAT_CLIP).values
    return z if kind == "z" else np.column_stack([ranked(raw, g["d"]).values, z])


def target(tr, kind):
    y = tr["y_raw"]
    if kind == "rank":
        return y.groupby(tr["d"]).rank(pct=True).values
    if kind == "z":
        return robust_z(y, tr["d"], Z_TARGET_CLIP).where(y.notna()).values
    n = y.groupby(tr["d"]).transform("count")
    return norm.ppf(((y.groupby(tr["d"]).rank() - 0.5) / n).values)


def fit(tr, kind, tkind):
    tr = with_rule_rank(tr)
    X = feats(tr, kind)
    y = target(tr, tkind)
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
        for name, (kind, tkind) in MODELS.items():
            models[name].append(fit(tr, kind, tkind))
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
    for v in [x for x in VARIANTS if x != "L"]:
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
