"""10/22 の判定の副: LBZ2（下限なし・2・3・5 億）− G の平常日を、入れ子の引き直しの t（tstat_nested）で day_boot と model_boot の両方から測る。

根拠: vault 20-research/2026-09-jp-daytrade-raw-feats.md の追記「気配の誤差の引き方をモデルにする」（2026-09-26）。
tstat_err はシード間のばらつきを丸ごと標準誤差に足すが、その中の「誤差を引き直す乱数の揺れ」は引いた本数で平均すれば縮む。
入れ子（材料の復元抽出 1 回ごとに誤差を 2 回引く）で、乱数の揺れと材料の不確かさを実測で分ける。直した t は
tstat_err と日だけの t の間に入るので、過去の不採用（日だけの t が基準未満）は変わらない。tstat_err 自体は変えない（過去の数字の再現のため）。

  PYTHONPATH=test test/.venv/bin/python test/dt_judge_sub.py --prelim   # 予備（slot 0859・9/24 まで、採否に使わない）
  PYTHONPATH=test test/.venv/bin/python test/dt_judge_sub.py            # 副の判定（材料 20 日から、dt_lbz2_floor.py の主と同じ日に）
  重いので bash test/heavy.sh … で回す。

事前登録（2026-09-26、結果を見る前に固定。この docstring を commit してから回す）:
  学習・本番の形・並べ方は test/dt_lbz2_floor.py と同じ（run_once をそのまま使う）。材料は判定なら 8:59:52 の層（sim.SNAP_SLOT）、
  最終日は回す日の前日。20 シード × 2 本（rep 0・1）。
  model_boot を使う前提: test/dt_err_model.py の当てはまりの確認（帯 2〜4 の |e| 平均と分位が実測の ±15%）を同じ材料で満たすこと。
    満たさなければ model_boot の副は出さない。
  副の判定（平常日の − G）: Z2F2・Z2F3・Z2F5 の 3 本で、差 > 0 かつ day_boot と model_boot の両方の tstat_nested ≥ 2.4 なら「副 ○」。
  副の役割: 主（dt_lbz2_floor.py）が条件 1（tstat_err ≥ 2.4）だけで欠け、ほかの条件をすべて満たし、副が ○ のとき、
    その形を「前向きの確認（本番の平常日に並べて記録だけ残す）に進める候補」としてユーザに出す。副だけで本番に入れることはしない。
  記述（採否に使わない）: Z2（下限なし）、tstat_err、日だけの t、材料の sd と乱数の sd。
"""

import argparse
import os
import sys

import numpy as np
import pandas as pd

os.environ["DT_AFFORD"] = "1"          # 単元の判定（dt_nscale.alloc_rule が見る）

from dt_err_model import check_fit  # noqa: E402
from dt_lbz2_floor import FLOORS, run_once  # noqa: E402
from dt_lgbm_bag import MODELS, fit, with_two_day  # noqa: E402
from dt_lgbm_uslow_model import NEED_DAYS, PRELIM_UNTIL, material_days  # noqa: E402
from dt_nscale import day_rules, tstat, tstat_err, tstat_nested  # noqa: E402
from dt_prefer_lgbm_wf import CAND, EMBARGO, FOLD_STARTS, LAST_DAY  # noqa: E402
import dt_preopen_sim as sim  # noqa: E402

T_MIN = 2.4
JUDGE = ("Z2F2", "Z2F3", "Z2F5")
REPS = 2


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--seeds", type=int, default=20)
    ap.add_argument("--prelim", action="store_true", help="予備: slot 0859・9/24 まで（採否に使わない）")
    a = ap.parse_args()

    n, last = material_days()
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
    modes = ["day_boot"] + (["model_boot"] if check_fit(slot) else [])
    if "model_boot" not in modes:
        print("model_boot は当てはまりを欠くので副に使わない")

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
    names = ["G", *FLOORS]

    def series(g):
        out, _, _ = run_once(g, models, fold_of.loc[g.index].values, rules, uslow)
        return {v: s.reindex(alld).fillna(0.0) for v, s in out.items()}

    res = {}
    for mode in modes:
        pools = sim.error_pools(slot, "2026-09-11", mode=mode)
        acc = {v: [[] for _ in range(seeds)] for v in names}
        for seed in range(seeds):
            for rep in range(REPS):
                out = series(sim.seen(te, sim.draw_errors(pools, te, seed, rep)))
                for v in names:
                    acc[v][seed].append(out[v])
            print(f"{mode} seed {seed}", flush=True)
        res[mode] = acc

    print(f"\n平常日 {normal.sum()} 日の − G（円/日）、{seeds} シード × {REPS} 本、材料 {slot}")
    passed = []
    for v in FLOORS:
        ts = {}
        for mode in modes:
            acc = res[mode]
            t, d, se_day, sd_p, sd_mc = tstat_nested(acc[v], acc["G"], normal)
            flat = lambda x: [s[0] for s in x]  # noqa: E731  rep 0 だけで従来の tstat_err
            t_old = tstat_err(flat(acc[v]), flat(acc["G"]), normal)
            x = np.mean([[(ai - bi).values[normal] for ai, bi in zip(ra, rb)] for ra, rb in zip(acc[v], acc["G"])], axis=(0, 1))
            ts[mode] = (t, d)
            print(f"  {v} {mode}: 差 {d:+,.0f}、tstat_nested {t:+.2f}（tstat_err {t_old:+.2f}・日だけ {tstat(x):+.2f}）、"
                  f"日の標本誤差 {se_day:,.0f}・材料の sd {sd_p:,.0f}・乱数の sd {sd_mc:,.0f}")
        if v in JUDGE:
            ok = len(ts) == 2 and all(d > 0 and t >= T_MIN for t, d in ts.values())
            print(f"    → 副 {'○' if ok else '×'}")
            if ok:
                passed.append(v)
    print("\n副: （予備なので出さない）" if a.prelim else
          "\n副: " + (f"{'・'.join(passed)} が ○（主が条件 1 だけで欠けたら前向きの確認の候補）" if passed else "どれも ×"))


if __name__ == "__main__":
    main()
