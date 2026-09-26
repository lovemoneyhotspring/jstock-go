"""気配の誤差の引き方をモデルにする（dt_preopen_sim の model・model_boot）。実測に合うかを確かめ、LBZ2 − G の t がどう変わるかを見る。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md（2026-09-26）。LBZ2 − G の平常日は日だけの t 2.57 が誤差込みで 1.50 に縮み、
縮みの主因は誤差の材料が 7 日しかないこと（day_boot で日を復元抽出するとシード間で結果がぶれる）。材料を眺めると、
銘柄を選ぶ帯（始値のギャップ −3〜0%）では日の中心のずれは標本の揺れと同じ大きさで、日の揺れは幅に出る。深い帯（−3% 以下）は
中心も幅も日で大きく動く。そこで誤差を「帯の中心 + 日の中心のずれ + 幅 × exp(日の幅のゆれ) × 標準化残差」に分け、
日数で決まるのは帯ごとの 4 つの数だけにし、分布の形（残差）は全日の数万行から取る（ユーザの提案）。

  PYTHONPATH=test test/.venv/bin/python test/dt_err_model.py --fit          # 当てはまりだけ（軽い）
  PYTHONPATH=test test/.venv/bin/python test/dt_err_model.py [--seeds 10]   # LBZ2 − G を 3 つの引き方で（重い）

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  当てはまりの確認（--fit）: 実測の行そのものに誤差を引き直し（50 シード）、帯ごとの |e| の平均・5/25/50/75/95% 分位・日ごとの log MAD の
    ばらつきを実測と並べる。帯 2〜4（銘柄を選ぶ帯）で |e| の平均と分位が実測の ±15% に入れば「形は合う」とする。
  LBZ2 − G（平常日）: 材料は予備と同じ slot 0859・9/24 まで。day_boot・model・model_boot を同じシード数で回し、差・日の標本誤差・
    シード間のばらつき・t を並べる（記述）。model_boot の差が day_boot の差の ±30% に入らなければ、モデルは誤差の効き方を変えてしまう
    （偏る）として、10/22 の判定の副には使わない。入れば、10/22 の判定で model_boot の t を副として並べる（主は day_boot のまま）。
"""

import argparse
import os

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_lgbm_bag import MODELS, feats, fit, with_two_day  # noqa: E402
from dt_lgbm_uslow_model import LIMIT, PRELIM_UNTIL  # noqa: E402
from dt_nscale import alloc_rule, calib_kappa, day_rules, pnl_day, tstat  # noqa: E402
from dt_prefer_lgbm_wf import CAND, CAP, EMBARGO, FOLD_STARTS, I0, LAST_DAY, RMAX, cap_and_rank  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

MODES = ("day_boot", "model", "model_boot")
QS = [5, 25, 50, 75, 95]


def check_fit(slot, n_sim=50):
    pools = sim.error_pools(slot, "2026-09-11", mode="model_boot")
    m, err = pools.model, pools.err
    print("\n推定（帯 0: 〜−5%・1: −5〜−3%・2: −3〜−1%・3: −1〜0%・4: 0〜3%・5: 3%〜）")
    print(pd.DataFrame({k: m[k] for k in ("mu", "s", "omega", "tau")}).assign(幅=lambda x: np.exp(x["s"])).round(3).to_string())
    te = pd.DataFrame({"d": err["d"].values, "gap": err["g"].values / 100})
    band = err["band"].values
    mad = lambda x: np.median(np.abs(x - np.median(x))) * 1.4826  # noqa: E731
    rows = []
    for mode in ("実測", "day_boot", "model", "model_boot"):
        sims = [err["e"].values] if mode == "実測" else []
        if mode != "実測":
            p = sim.ErrorPools(err, mode)
            sims = [sim.draw_errors(p, te, seed) for seed in range(n_sim)]
        for b in range(len(sim.BANDS) - 1):
            sel = band == b
            if not sel.any():
                continue
            a = np.stack([x[sel] for x in sims])
            lm = [np.std([np.log(mad(x[sel & (err["d"].values == d)]) + 1e-9) for d in pools.days]) for x in sims[:10]]
            rows.append({"引き方": mode, "帯": b, "|e| 平均": np.abs(a).mean(),
                         **{f"{q}%": np.percentile(a, q, axis=1).mean() for q in QS}, "日の logMAD の sd": np.mean(lm)})
    t = pd.DataFrame(rows).set_index(["帯", "引き方"]).sort_index()
    print("\n当てはまり（帯 × 引き方、誤差 %pt）")
    print(t.round(3).to_string())
    obs = t.xs("実測", level="引き方")
    for mode in ("model", "model_boot"):
        r = t.xs(mode, level="引き方").loc[[2, 3, 4]]
        o = obs.loc[[2, 3, 4]]
        cols = ["|e| 平均", "5%", "25%", "75%", "95%"]
        worst = (r[cols] / o[cols] - 1).abs().max().max()
        print(f"  {mode}: 帯 2〜4 の |e| 平均と分位（中央値を除く）の実測との差の最大 {worst * 100:.1f}% → "
              + ("形は合う" if worst <= 0.15 else "合わない"))


def compare(slot, seeds):
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

    def diff(g):
        gf = fold_of.loc[g.index].values
        s = np.empty(len(g))
        for k, m in enumerate(models):
            s[gf == k] = m.predict(feats(g[gf == k], kind))
        by = {"G": cap_and_rank(g, -g["key_sort"].values), "Z2": cap_and_rank(g, s)}
        kappa = calib_kappa(by["G"], I0)
        out = {}
        for v, x in by.items():
            pn = {}
            for d, y in x.groupby("d"):
                idx, w = alloc_rule(y, 0.002, RMAX, CAP * rules.loc[d, "mult"], k=7)
                if d in uslow:
                    ok = y.loc[idx, "gap_true"].values <= LIMIT
                    idx, w = idx[ok], w[ok]
                pn[d] = pnl_day(y, idx, w, kappa) if len(idx) else 0.0
            out[v] = pd.Series(pn).reindex(alld).fillna(0.0)
        return (out["Z2"] - out["G"]).values[normal]

    print(f"\nLBZ2 − G（平常日 {normal.sum()} 日、1,000 万、円/日）、{seeds} シード")
    res = {}
    for mode in MODES:
        pools = sim.error_pools(slot, "2026-09-11", mode=mode)
        D = np.stack([diff(sim.seen(te, sim.draw_errors(pools, te, seed))) for seed in range(seeds)], axis=1)
        x = D.mean(axis=1)
        se_day, sd_seed = x.std(ddof=1) / np.sqrt(len(x)), D.mean(axis=0).std(ddof=1)
        res[mode] = x.mean()
        print(f"  {mode}: 差 {x.mean():+,.0f}、日の標本誤差 {se_day:,.0f}、シード間 {sd_seed:,.0f}（シードごと "
              + " ".join(f"{v:+,.0f}" for v in D.mean(axis=0)) + f"） → t {x.mean() / np.hypot(se_day, sd_seed):+.2f}"
              f"（日だけ {tstat(x):+.2f}）", flush=True)
    ok = abs(res["model_boot"] / res["day_boot"] - 1) <= 0.30
    print(f"  model_boot の差は day_boot の {res['model_boot'] / res['day_boot'] * 100:.0f}% → "
          + ("10/22 の判定の副に使う" if ok else "偏るので副に使わない"))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=10)
    ap.add_argument("--fit", action="store_true", help="当てはまりだけ")
    a = ap.parse_args()
    os.environ["DT_ERR_UNTIL"] = PRELIM_UNTIL
    check_fit("0859")
    if not a.fit:
        compare("0859", a.seeds)


if __name__ == "__main__":
    main()
