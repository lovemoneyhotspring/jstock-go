"""平常日の並べ方（gap_vol か LightGBM か）を、発注時の気配の誤差で測り直す（rankby の見直し）。

根拠: vault 30-projects/daytrade.md「気配の誤差の模擬に頼る決定の見直し」、
      20-research/2026-09-jp-daytrade-rankby-preopen-fair.md（旧い形 N=3・逆ボラで平常日 gap_vol に決めた。誤差なしでは逆）、
      20-research/2026-09-jp-daytrade-oscillator.md（今の形の walk-forward で平常日も L − G +3,654 円/日、t 2.26。記述）

  PYTHONPATH=test test/.venv/bin/python test/dt_rankby_order.py [--seeds 10]

事前登録（2026-09-25、発注時の気配が 1 日分しか無いうちに固定。この docstring を commit してから回す）:
  回す日: 発注時の気配（history/quotes の 8:59 台）と始値がそろった日が **10 日**になってから（2026-09-24 から数えて
          10/7 の始値が入った後）。10 日に満たなければ止める（下の MIN_DAYS）。ユーザ判断で 10 日（2026-09-25）。
  誤差: 見える気配 − 始値。材料は dt_preopen_sim.error_frame("snap")（2026-09-25 ユーザ判断で決め直し。結果はまだ見ていない）:
          第一 8:59:48 以降の値（候補は発注時の気配、候補の外は寄る前の open が撮る板）、
          補い 第一に無い銘柄だけ同じ日の 8:59:30 の snap（slot 085930）。
        ギャップの帯（BANDS）ごとに引く。行が 30 未満の帯は、30 以上ある帯のうち帯の番号が最も近いものの行で代える。
        回す日の数え方（10 日）は第一の材料のある日。
        記述: 発注時の気配だけ（候補の外を入れない）でも回して並べる。
    iid    帯ごとに全日の行から引く（主）
    block  模擬の 1 日ごとに記録の 1 日を選び、その日のその帯の行から引く（5 行未満ならその帯の全日の行）
    upper  誤差なし（記述）
  学習: test/dt_prefer_lgbm_wf.py と同じ walk-forward 7 本（2019-09-17 から 1 年ずつ、embargo 5 日）。モデルは今の 16 特徴量（M0）。
  本番の形: 業種の上限 1、規則 R（p 0.2%・資金 ÷ 7・10 位まで＝max_positions）、1,000 万、I0 10 bp、12 月除く、10 シード。
  並べ方: L（M0 の予測値の高い順）、G（gap_vol）。
  日の区分: 平常日 = dt_nscale.day_rules で米国小幅高でない日（本番の平常日は rank_by = gap_vol）。
    【2026-09-25 決め直し、結果を見る前】判定は本番と同じ S&P500 0〜+1% かつ VIX ≤ 24（day_rules の us="spx"）。
    それまでの既定（ナスダック近似）は本番と 487 日食い違っていた（vault 20-research/2026-09-jp-daytrade-sim-parity.md）
  判定（平常日の L − G、シードで平均した日次の差。誤差の帯は上の「主」の代え方）:
    iid で t ≥ 2 かつ block で差 > 0 → 平常日を LightGBM にする案をユーザに出す（rank_by = "lgbm"）
    iid で t ≤ −2 かつ block で差 < 0 → gap_vol のままを確かめた
    それ以外                          → 判断しない（今のまま。記録が 20 日で測り直す）
  記述: 米国小幅高の日の L − G、upper、年ごと。
"""

import argparse

import numpy as np
import pandas as pd

from dt_nscale import alloc_rule, calib_kappa, day_rules, max_dd, pnl_day, tstat
from dt_prefer_lgbm_wf import CAND, cap_and_rank, feats, fit
from dt_preopen_sim import BANDS, error_frame, seen
from dt_wf_target import EMBARGO, FOLD_STARTS

MIN_DAYS = 10
MIN_BAND_ROWS = 30
RMAX, CAP, I0 = 10, 1e7, 10e-4


def band_pools(err):
    """帯ごとの誤差の行（d・e）。行が MIN_BAND_ROWS 未満の帯は近い帯で代える。返すのは {帯: DataFrame(d, e)} と出どころの説明。"""
    nb = len(BANDS) - 1
    err = err.assign(band=np.digitize(err["g"], BANDS[1:-1], right=True))
    ok = [b for b in range(nb) if (err["band"] == b).sum() >= MIN_BAND_ROWS]
    if not ok:
        raise SystemExit("誤差の行が足りない")
    pools, how = {}, {}
    for b in range(nb):
        src = b if b in ok else min(ok, key=lambda o: (abs(o - b), o))
        pools[b] = err.loc[err["band"] == src, ["d", "e"]]
        how[b] = "そのまま" if src == b else f"帯 {src} で代える"
    return pools, how


def draw(rng, pools, band, day_idx, rec_days, mode):
    e = np.zeros(len(band))
    if mode == "upper":
        return e
    pick = rng.integers(len(rec_days), size=day_idx.max() + 1)[day_idx] if mode == "block" else None
    for b in np.unique(band):
        pool_all = pools[b]["e"].values
        if mode == "iid":
            e[band == b] = rng.choice(pool_all, size=int((band == b).sum()))
            continue
        for k, rd in enumerate(rec_days):
            m = (band == b) & (pick == k)
            if not m.any():
                continue
            pool = pools[b].loc[pools[b]["d"] == rd, "e"].values
            e[m] = rng.choice(pool if len(pool) >= 5 else pool_all, size=int(m.sum()))
    return e


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    a = ap.parse_args()

    err = error_frame("snap", "2026-09-19")  # 第一の材料が 10 日に満たなければここで止まる（MIN_DAYS と同じ 10）
    first_days = err.loc[err["src"] != "085930", "d"].nunique()
    print(f"誤差の材料: 第一の材料のある日 {first_days} 日、{len(err):,} 行（出どころ {err['src'].value_counts().to_dict()}）", flush=True)
    if first_days < MIN_DAYS:
        raise SystemExit(f"{MIN_DAYS} 日に満たないので回さない（事前登録）")
    rec_days = np.sort(err["d"].unique())
    order_only = err[err["src"] == "order"]
    forms = {}
    for form, frame in (("主", err), ("発注時の気配だけ（記述）", order_only)):
        forms[form], how = band_pools(frame)
        print(f"帯（{form}）: " + "、".join(f"{b}={h}（{len(forms[form][b])} 行）" for b, h in how.items()))

    df = pd.read_parquet(CAND)
    last_day = df["d"].max()
    df = df.copy()
    df["price"] = df["o"]
    df["sector"] = df["sector"].fillna("")
    days = np.array(sorted(df["d"].unique()))
    bounds = [pd.Timestamp(s) for s in FOLD_STARTS] + [pd.Timestamp(last_day) + pd.Timedelta(days=1)]
    models = []
    for k in range(len(FOLD_STARTS)):
        train_days = days[days < bounds[k]][:-EMBARGO]
        models.append(fit(df[df["d"].isin(train_days) & (df["gap"] < 0)], False))
        print(f"{FOLD_STARTS[k]}: 木 {models[-1].n_estimators} 本", flush=True)

    te = df[(df["d"] >= bounds[0]) & (df["d"].dt.month != 12)].copy()
    fold_of = pd.Series(np.searchsorted(np.array(bounds[1:], dtype="datetime64[ns]"), te["d"].values, side="right"), index=te.index)
    rules = day_rules(te, us="spx", skip_months=(12,))   # 本番と同じ区分（2026-09-25、回す前に決め直し）
    alld = pd.DatetimeIndex(sorted(te["d"].unique()))
    band = np.digitize(te["gap"].values * 100, BANDS[1:-1], right=True)
    day_idx = te["d"].rank(method="dense").astype(int).values - 1
    acc = {}
    runs = [("iid", "主"), ("block", "主"), ("upper", "主"), ("iid", "発注時の気配だけ（記述）")]
    for mode, form in runs:
        for seed in range(a.seeds if mode != "upper" else 1):
            g = seen(te, draw(np.random.default_rng(seed), forms[form], band, day_idx, rec_days, mode))
            gf = fold_of.loc[g.index].values
            X = feats(g, False)
            s = np.empty(len(g))
            for k, m in enumerate(models):
                s[gf == k] = m.predict(X[gf == k])
            rk = {"L": cap_and_rank(g, s), "G": cap_and_rank(g, -g["key_sort"].values)}
            kappa = calib_kappa(rk["G"], I0)
            for v, x in rk.items():
                ser = pd.Series({d: pnl_day(y, *alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7), kappa)
                                 for d, y in x.groupby("d")})
                acc.setdefault((mode, form, v), []).append(ser.reindex(alld).fillna(0.0))
            print(f"{mode}/{form} seed {seed}", flush=True)

    uslow = rules.reindex(alld)["skip"].fillna(False).values
    print(f"\n検証期間 {alld.min():%Y-%m-%d}〜{alld.max():%Y-%m-%d}、{len(alld)} 日（平常日 {(~uslow).sum()}・米国小幅高 {uslow.sum()}）、"
          f"資金 {CAP / 1e4:.0f} 万・{RMAX} 位まで・{a.seeds} シード（円/日）")
    print("| 誤差 | 日 | L | G | L − G（t） | L 年率・最大DD | G 年率・最大DD |")
    print("|---|---|---|---|---|---|---|")
    for mode, form in runs:
        L, G = (pd.concat(acc[(mode, form, v)], axis=1).mean(axis=1) for v in ("L", "G"))
        for name, m in (("平常日（判定）", ~uslow), ("米国小幅高の日", uslow)):
            d = (L - G)[m]
            ann = lambda v: np.mean([x[m].mean() * 245 / CAP * 100 for x in acc[(mode, form, v)]])
            dd = lambda v: np.mean([max_dd(x[m].values) / CAP * 100 for x in acc[(mode, form, v)]])
            print(f"| {mode}・{form} | {name} | {L[m].mean():,.0f} | {G[m].mean():,.0f} | {d.mean():+,.0f}（{tstat(d):+.2f}） | "
                  f"{ann('L'):.1f}%・{dd('L'):.1f}% | {ann('G'):.1f}%・{dd('G'):.1f}% |")
    L, G = (pd.concat(acc[("iid", "主", v)], axis=1).mean(axis=1) for v in ("L", "G"))
    print("\n平常日の年ごとの L − G（iid・主、円/日）: " + " ".join(
        f"{y}:{(L - G)[(~uslow) & (alld.year == y)].mean():+,.0f}" for y in sorted(set(alld.year))))


if __name__ == "__main__":
    main()
