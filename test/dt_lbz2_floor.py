"""LBZ2 にだけ売買代金の下限を掛け、平常日の上乗せが小さい銘柄から来ていないかを見る。あわせて LBZ2 − G の標準誤差の内訳を測る。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記「LBZ2 と gap_vol の詳しい比較」（2026-09-26）。LBZ2 は売買代金の
中央値 4.3 億（G 9.8 億）の銘柄に寄り、LBZ2 だけが選ぶ銘柄（中央値 3.8 億）が +23.1 bp で上乗せの源。滑りは板で測ると
ティックの約 5 倍なので、小さい銘柄の上乗せは本番で削れうる。G の下限は 1 億で決着済み（3 億以上は負ける）だが、
LBZ2 は選ぶ銘柄が違うので LBZ2 にだけ掛けて測る（ユーザの指示）。

  PYTHONPATH=test test/.venv/bin/python test/dt_lbz2_floor.py --prelim   # 予備（9/24 までの 0859、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_lbz2_floor.py            # 判定（材料 20 日から）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習は test/dt_lgbm_bag.py の BZ2（bagging 5 本・順位の特徴量 16 ＋ r_d2・A・日ごとの z の教師データ）。下限は学習に掛けず、
  並べる前にその日の候補から売買代金（turnover_med）が下限未満の銘柄を外す。G は今のまま（候補表の 1 億）。本番の形は dt_lgbm_bag と同じ。
  並べ方: Z2（下限なし）・Z2F2（2 億）・Z2F3（3 億）・Z2F5（5 億）
  判定（平常日の − G）: Z2F2・Z2F3・Z2F5 の 3 本に dt_lgbm_gauss の条件 1〜5 を当てる（1 の t ≥ 2.4）。加えて 6. Z2 − G の半分以上を残す。
    満たした下限のうち一番高いものを「LBZ2 に掛ける下限」とする。どれも満たさなければ、上乗せは小さい銘柄頼みとして下限なしの判定に戻る。
  記述（採否に使わない）: Z2F − Z2、1 日の建玉、LBZ2 − G の標準誤差の内訳（日の標本誤差・誤差の材料のシード間）、
    地合い（その日の候補全体の寄→引）で調整した t、日ごとの順位相関（IC、全候補）の差の t、誤差の材料が増えたときの t の目安。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_bag import MODELS, feats, fit, with_two_day  # noqa: E402
from dt_lgbm_gauss import SCALES, SEEDS_OK  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, NEED_DAYS, PRELIM_UNTIL, SPLIT, material_days  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat, tstat_err  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
from dt_wf_target import liq_cost_bp  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

T_MIN = 2.4
FLOORS = {"Z2": 0.0, "Z2F2": 2e8, "Z2F3": 3e8, "Z2F5": 5e8}
JUDGE = ("Z2F2", "Z2F3", "Z2F5")
ERR_DAYS = 7                           # 予備の誤差の材料の日数（目安の計算に使う）


def run_once(g, models, gf, rules, uslow, liq=False):
    """並べ方ごとの日次損益と、その日の全候補での順位相関（IC）を返す。"""
    kind = MODELS["BZ2"][0]
    s = np.empty(len(g))
    for k, m in enumerate(models):
        s[gf == k] = m.predict(feats(g[gf == k], kind))
    ic = {"G": g.assign(_s=-g["key_sort"].values), "Z2": g.assign(_s=s)}
    ic = {v: x.groupby("d")[["_s", "y_raw"]].apply(lambda y: y["_s"].corr(y["y_raw"], method="spearman")) for v, x in ic.items()}
    if liq:
        g = g.assign(y_raw=g["y_raw"] - (liq_cost_bp(g["turnover_med"].values) - 5.7) / 1e4)
    by = {"G": cap_and_rank(g, -g["key_sort"].values)}
    for v, fl in FLOORS.items():
        keep = g["turnover_med"].values >= fl
        by[v] = cap_and_rank(g[keep], s[keep])
    kappa = calib_kappa(by["G"], I0)
    out, amt = {}, {}
    for v, x in by.items():
        pn, am = {}, {}
        for d, y in x.groupby("d"):
            idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
            if d in uslow:   # 小幅高の日は寄指 −1.5%
                ok = y.loc[idx, "gap_true"].values <= LIMIT
                idx, w = idx[ok], w[ok]
            pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
            am[d] = float(np.sum(w))
        out[v], amt[v] = pd.Series(pn), pd.Series(am)
    return out, amt, ic


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
        slot, seeds, err_days = "0859", min(a.seeds, 10), ERR_DAYS
        print("**予備: 誤差の材料は slot 0859・9/24 まで（7 日）。採否に使わない**")
    else:
        if n < NEED_DAYS:
            sys.exit(f"材料が {NEED_DAYS} 日に満たないので回さない（予備は --prelim）")
        os.environ["DT_ERR_UNTIL"] = last
        os.environ.pop("DT_ERR_SLOT", None)
        slot, seeds, err_days = sim.SNAP_SLOT, a.seeds, n

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
    mkt = te.groupby("d")["y_raw"].mean().reindex(alld)

    def series(g, liq=False):
        out, amt, ic = run_once(g, models, fold_of.loc[g.index].values, rules, uslow, liq)
        f = lambda s: s.reindex(alld).fillna(0.0)  # noqa: E731
        return {v: f(s) for v, s in out.items()}, {v: f(s) for v, s in amt.items()}, {v: s.reindex(alld) for v, s in ic.items()}

    upper, _, _ = series(sim.seen(te, np.zeros(len(te))))
    pools = sim.error_pools(slot, "2026-09-11", mode="day_boot")
    names = ["G", *FLOORS]
    acc = {sc: {v: [] for v in names} for sc in (1.0,) + SCALES}
    liqc = {v: [] for v in names}
    amts = {v: [] for v in names}
    ics = {"G": [], "Z2": []}
    for seed in range(seeds):
        e = sim.draw_errors(pools, te, seed)
        for sc in acc:
            out, amt, ic = series(sim.seen(te, e * sc))
            for v in names:
                acc[sc][v].append(out[v])
            if sc == 1.0:
                for v in names:
                    amts[v].append(amt[v])
                for v in ics:
                    ics[v].append(ic[v])
        out, _, _ = series(sim.seen(te, e), liq=True)
        for v in names:
            liqc[v].append(out[v])
        print(f"seed {seed}", flush=True)

    avg = lambda xs: pd.concat(xs, axis=1).mean(axis=1)  # noqa: E731
    base = acc[1.0]
    mean = {v: avg(x) for v, x in base.items()}
    lq = {v: avg(x) for v, x in liqc.items()}
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、平常日 {normal.sum()} 日、資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・"
          f"{seeds} シード（円/日）")

    print("\n平常日の − G")
    ref = (mean["Z2"] - mean["G"])[normal].mean()
    passed = []
    for v in FLOORS:
        d = (mean[v] - mean["G"])[normal]
        te_ = tstat_err(base[v], base["G"], normal)
        pos = sum((x - g_)[normal].mean() > 0 for x, g_ in zip(base[v], base["G"]))
        first, second = d[d.index < SPLIT].mean(), d[d.index >= SPLIT].mean()
        scaled = [(avg(acc[sc][v]) - avg(acc[sc]["G"]))[normal].mean() for sc in SCALES]
        d_lq = (lq[v] - lq["G"])[normal].mean()
        up = (upper[v] - upper["G"])[normal].mean()
        checks = [d.mean() > 0 and te_ >= T_MIN, pos >= SEEDS_OK * seeds, first > 0 and second > 0,
                  all(s > 0 for s in scaled), d_lq > 0, d.mean() >= ref / 2]
        mark = "（下限なし）" if v == "Z2" else ("○" if all(checks) else "×")
        print(f"  {v} − G: {d.mean():+,.0f}（日の t {tstat(d):+.2f}／誤差込み t {te_:+.2f}）、正のシード {pos}/{seeds}、"
              f"前半 {first:+,.0f}・後半 {second:+,.0f}、誤差 0.8/1.2 倍 {scaled[0]:+,.0f}/{scaled[1]:+,.0f}、"
              f"流動性コスト込み {d_lq:+,.0f}、誤差なし {up:+,.0f}、1 日の建玉 {avg(amts[v])[normal].mean() / 1e4:,.0f} 万 → {mark} "
              + "".join("○" if c else "×" for c in checks))
        yr = d.index.year
        print("    年ごと: " + " ".join(f"{y}:{d[yr == y].mean():+,.0f}" for y in sorted(set(yr))))
        if v != "Z2" and all(checks):
            passed.append((FLOORS[v], v))
    print("\n判定: （予備なので出さない）" if a.prelim else
          "\n判定: " + (f"LBZ2 に {max(passed)[0] / 1e8:.0f} 億の下限を掛ける（{max(passed)[1]}）" if passed
                      else "上乗せは小さい銘柄頼み。下限なしの判定に戻る"))

    print("\n記述: Z2 − G の標準誤差の内訳（平常日）")
    D = pd.concat(base["Z2"], axis=1).values[normal] - pd.concat(base["G"], axis=1).values[normal]
    x = D.mean(axis=1)
    se_day = x.std(ddof=1) / np.sqrt(len(x))
    sd_seed = D.mean(axis=0).std(ddof=1)
    print(f"  差 {x.mean():+,.0f}、日の標本誤差 {se_day:,.0f}、誤差の材料のシード間 {sd_seed:,.0f} → t {x.mean() / np.hypot(se_day, sd_seed):+.2f}"
          f"（日だけなら {x.mean() / se_day:+.2f}）")
    m = mkt[normal].values - mkt[normal].values.mean()
    b = np.polyfit(m, x, 1)[0]
    res = x - x.mean() - b * m
    se_adj = res.std(ddof=2) / np.sqrt(len(x))
    print(f"  地合いで調整（差 = a + b × 候補全体の寄→引、b {b:,.0f}）: 日の標本誤差 {se_day:,.0f} → {se_adj:,.0f}、"
          f"t {x.mean() / np.hypot(se_adj, sd_seed):+.2f}（日だけなら {x.mean() / se_adj:+.2f}）")
    for k in (20, 40, 60):
        sd_k = sd_seed * np.sqrt(err_days / k)
        print(f"  誤差の材料 {k} 日の目安（シード間が √日数 で縮むと置く）: t {x.mean() / np.hypot(se_day, sd_k):+.2f}・"
              f"地合い調整 {x.mean() / np.hypot(se_adj, sd_k):+.2f}")
    icd = pd.concat(ics["Z2"], axis=1)[normal] - pd.concat(ics["G"], axis=1)[normal]
    icx = icd.mean(axis=1)
    ic_seed = icd.mean(axis=0).std(ddof=1)
    print(f"  日ごとの順位相関（全候補）: G {avg(ics['G'])[normal].mean():+.4f}・Z2 {avg(ics['Z2'])[normal].mean():+.4f}、"
          f"差の t {icx.mean() / np.hypot(icx.std(ddof=1) / np.sqrt(icx.notna().sum()), ic_seed):+.2f}"
          f"（日だけなら {tstat(icx):+.2f}）、差が正の日 {(icx > 0).mean() * 100:.1f}%")
    for v in names:
        print(f"  {v}: 全日の年率 {np.mean([s.mean() * 245 / CAP * 100 for s in base[v]]):.1f}%・最大DD "
              f"{np.mean([max_dd(s.values) / CAP * 100 for s in base[v]]):.1f}%")


if __name__ == "__main__":
    main()
