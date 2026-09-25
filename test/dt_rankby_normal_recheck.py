"""平常日（本番が gap_vol で並べる日）を LightGBM で並べたら良くなるか。今の本番の形と発注時の気配の誤差で測り直す。

根拠: vault 20-research/2026-09-jp-daytrade-rankby-preopen-fair.md（9/20、旧い形 N=3・逆ボラで G0 が L に +5.16 bp、
平常日は gap_vol に決めた。誤差なしでは逆向き）と 2026-09-jp-daytrade-oscillator.md（9/25、規則 R・10 位・1,000 万の
walk-forward で平常日も L − G +3,654、t 2.26、記述）、2026-09-jp-daytrade-remeasure-afford-err.md（単元の判定と流動性別
コストで lgbm 優位が t 1.65 に縮み、逆転）。向きが気配の誤差の入れ方で変わるので、誤差の材料が溜まってから 1 度だけ回す。

  test/.venv/bin/python test/dt_candidates.py --max-gap 0.03 --out test/out/dt_candidates_wide.parquet
  PYTHONPATH=test test/.venv/bin/python test/dt_rankby_normal_recheck.py --check   # 材料の日数だけ
  PYTHONPATH=test test/.venv/bin/python test/dt_rankby_normal_recheck.py           # 回す（20 シード）

事前登録（2026-09-25 夜、結果を見る前に固定。この docstring を commit してから回す。9/20 の「LightGBM に戻す条件」を今の形に決め直したもの）:
  学習: test/dt_prefer_lgbm_wf.py と同じ walk-forward（FOLD_STARTS 7 本・EMBARGO 5 日・KW・内部検証 250 日の early stopping）の
        M0（今の特徴量 16 個）。検証期間 2019-09-17〜2026-09-15、12 月は除く。
  並べ方: L = M0 の予測値の高い順、G = gap_vol（見えるギャップ ÷ vol20）。どちらも業種の上限 1 を掛ける。
  本番の形: 日の区分は本番（day_rules(us="spx", skip_months=(12,))）、単元の判定あり（DT_AFFORD=1）、規則 R（p 0.2%・資金 ÷ 7・
           10 位まで）で 1,000 万（ショック日は ×1.5）、平方根則の滑り I0 10 bp（ほかの検証と同じ）。
  気配の誤差: 層にした既定の材料（8:59:52 の発注時の気配 → 8:59:48 以降の板 → 8:59:30）を day_boot で引く。
             第一・第二の層のある日が 20 日以上あるときだけ回す。最終日は回す日の前日に固定（DT_ERR_UNTIL）。20 シード。
  対象の日（主）: 平常日 = 米国小幅高でも 12 月でもない日（ショック日を含む）。
  判定（主）: 平常日の L − G（シードで平均した日次の差）が次をすべて満たせば「平常日も LightGBM に戻す」。1 本の比較なので補正なし。
    1. 差 > 0 かつ誤差込みの t（tstat_err）≥ 2.0
    2. 20 シード中 16 以上で差 > 0
    3. 前半（2019-09-17〜2022-12-31）と後半（2023-01-01〜）の両方で差 > 0
    4. 誤差を 0.8 倍・1.2 倍にしても差 > 0（向きが誤差の大きさに依らない）
    5. 流動性別コスト（liq_cost_bp の 5.7 bp を超える分、売買代金 3 億未満で往復 +20 bp）を上乗せしても差 > 0
       （9/25 夜に lgbm の優位がこれで逆転した。水準は厳しすぎるので主の形には入れず、向きだけ見る）
  満たさなければ gap_vol のまま閉じる（以後、この比較は回さない）。
  記述（採否に使わない）: 日の t、誤差なしの差、小幅高の日の L − G、年ごと、年率と最大 DD。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank, feats, fit  # noqa: E402
from dt_wf_target import liq_cost_bp  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

NEED_DAYS = 20
SEEDS_OK = 16
SPLIT = pd.Timestamp("2023-01-01")
SCALES = (0.8, 1.2)


def material_days():
    """層の第一・第二（8:59:48 以降）の材料のある日数と最終日。"""
    keep, sim.MIN_SNAP_DAYS = sim.MIN_SNAP_DAYS, 0
    try:
        e = sim.error_frame("snap", "2026-09-11")
    finally:
        sim.MIN_SNAP_DAYS = keep
    first = e.loc[e["src"] != "085930", "d"]
    return first.nunique(), (pd.Timestamp(first.max()).strftime("%Y-%m-%d") if len(first) else None)


def run_once(g, models, gf, rules, liq=False):
    s = np.empty(len(g))
    for k, m in enumerate(models):
        s[gf == k] = m.predict(feats(g[gf == k], False))
    if liq:
        g = g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
    by = {"L": cap_and_rank(g, s), "G": cap_and_rank(g, -g["key_sort"].values)}
    kappa = calib_kappa(by["G"], I0)   # dt_prefer_lgbm_wf・dt_lgbm_rsi2 と同じく、並べた G で毎回合わせる
    return {v: pd.Series({d: pnl_day(y, *alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7), kappa)
                          for d, y in x.groupby("d")}) for v, x in by.items()}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--check", action="store_true", help="材料の日数を見るだけ")
    a = ap.parse_args()

    n, last = material_days()
    print(f"8:59:48 以降の材料のある日: {n} 日（最終 {last}）、判定に要る日数 {NEED_DAYS}")
    if a.check:
        print("回してよい" if n >= NEED_DAYS else f"まだ（あと {NEED_DAYS - n} 日）")
        return
    if n < NEED_DAYS:
        sys.exit(f"材料が {NEED_DAYS} 日に満たないので回さない")
    os.environ["DT_ERR_UNTIL"] = last
    os.environ.pop("DT_ERR_SLOT", None)

    df = pd.read_parquet(CAND)
    df = df[df["d"] <= LAST_DAY].copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(LAST_DAY) + pd.Timedelta(days=1)]
    models = []
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)], False))
        print(f"{FOLD_STARTS[k]}: 木 {models[-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    uslow = rules.reindex(alld)["us_low"].fillna(False).values.astype(bool)
    normal = ~uslow

    def series(g, liq=False):
        out = run_once(g, models, fold_of.loc[g.index].values, rules, liq)
        return {v: s.reindex(alld).fillna(0.0) for v, s in out.items()}

    g0 = sim.seen(te, np.zeros(len(te)))
    upper = series(g0)
    pools = sim.error_pools(sim.SNAP_SLOT, "2026-09-11", mode="day_boot")
    acc = {sc: {"L": [], "G": []} for sc in (1.0,) + SCALES}
    liqc = {"L": [], "G": []}
    for seed in range(a.seeds):
        e = sim.draw_errors(pools, te, seed)
        for sc in acc:
            out = series(sim.seen(te, e * sc))
            for v in out:
                acc[sc][v].append(out[v])
        out = series(sim.seen(te, e), liq=True)
        for v in out:
            liqc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    base = acc[1.0]
    mean = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in base.items()}
    diff = mean["L"] - mean["G"]
    d_n = diff[normal]
    t_err = tstat_err(base["L"], base["G"], normal)
    seed_pos = sum((l_ - g_)[normal].mean() > 0 for l_, g_ in zip(base["L"], base["G"]))
    first, second = d_n[d_n.index < SPLIT].mean(), d_n[d_n.index >= SPLIT].mean()
    scaled = {sc: (pd.concat(acc[sc]["L"], axis=1).mean(axis=1) - pd.concat(acc[sc]["G"], axis=1).mean(axis=1))[normal].mean()
              for sc in SCALES}

    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（平常日 {normal.sum()} 日）、"
          f"誤差の材料 {n} 日（{last} まで）、資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{a.seeds} シード（円/日）")
    checks = [
        (f"1. 差 {d_n.mean():+,.0f}、誤差込み t {t_err:+.2f}（≥ 2.0）", d_n.mean() > 0 and t_err >= 2.0),
        (f"2. 差 > 0 のシード {seed_pos}/{a.seeds}（≥ {SEEDS_OK}）", seed_pos >= SEEDS_OK),
        (f"3. 前半 {first:+,.0f}・後半 {second:+,.0f}（両方 > 0）", first > 0 and second > 0),
        ("4. 誤差 " + "・".join(f"{sc} 倍 {v:+,.0f}" for sc, v in scaled.items()) + "（どちらも > 0）",
         all(v > 0 for v in scaled.values())),
    ]
    lq = {v: pd.concat(x, axis=1).mean(axis=1) for v, x in liqc.items()}
    d_lq = (lq["L"] - lq["G"])[normal].mean()
    checks.append((f"5. 流動性別コストを載せた差 {d_lq:+,.0f}（> 0）", d_lq > 0))
    for label, ok in checks:
        print(f"  {'○' if ok else '×'} {label}")
    print("判定: " + ("平常日も LightGBM に戻す" if all(ok for _, ok in checks) else "gap_vol のまま閉じる"))

    print("\n記述（採否に使わない）:")
    print(f"  平常日: 日の t {tstat(d_n):+.2f}、誤差なしの差 {(upper['L'] - upper['G'])[normal].mean():+,.0f}")
    print(f"  米国小幅高の日の L − G {diff[uslow].mean():+,.0f}（誤差込み t {tstat_err(base['L'], base['G'], uslow):+.2f}）")
    yr = alld.year
    print("  平常日の年ごと: " + " ".join(f"{y}:{diff[normal & (yr == y)].mean():+,.0f}" for y in sorted(set(yr))))
    for v in ("L", "G"):
        print(f"  {v}: 全日の年率 {np.mean([x.mean() * 245 / CAP * 100 for x in base[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(x.values) / CAP * 100 for x in base[v]]):.1f}%")


if __name__ == "__main__":
    main()
