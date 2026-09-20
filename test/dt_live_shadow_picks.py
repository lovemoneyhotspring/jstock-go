"""本番が選定に使った回の気配で、lgbm / gap_vol を本番の形（4 銘柄・業種 1 銘柄）で並べ直し、銘柄ごとの損益を出す。

dt_live_shadow.py は「その日の最初の回・上位 3・逆ボラの重み」で bp を比べる。こちらは「円でいくらだったか」を
銘柄ごとに答えるためのもの。回は evaluation が採点に使った順位表の回（ranking_run_id）に合わせ、
損益は evaluation の行をそのまま引く（hypo_pnl = 選んだ銘柄は記録の株数、それ以外は予算で買える株数。
even_pnl = 全行を等金額に揃えた想定。2026-09-18 から）。gap_vol 側が本番の選定を再現するかが照合になる。

  test/.venv/bin/python test/dt_live_shadow_picks.py [--since 2026-09-15] [--n 4]
"""

import argparse

import duckdb
import lightgbm as lgb
import numpy as np
import pandas as pd

from dt_candidates import limit_down
from dt_lgbm_train import ranked, raw_features
from dt_preopen_sim import with_rule_rank

HIST = "state/daytrade/history"

SQL = f"""
WITH e AS (
  SELECT * FROM read_parquet('{HIST}/evaluation/*.parquet', union_by_name=true)
  WHERE side = 'BUY' AND day >= CAST(? AS DATE) AND ranking_run_id IS NOT NULL
  QUALIFY recorded_at = max(recorded_at) OVER (PARTITION BY day)),
p AS (
  SELECT * FROM read_parquet('{HIST}/plan/*.parquet', union_by_name=true)
  QUALIFY recorded_at = max(recorded_at) OVER (PARTITION BY day))
SELECT e.day d, e.symbol code, e.name, q.price, q.prev_close, q.usable, e.budget, e.picked, e.actual_pnl,
       e.gap_open, e.ret_oc, e.net_bp, e.hypo_pnl, e.even_pnl, e.cost_bp, e.open, e.hypo_quantity, e.even_quantity,
       p.eligible, p.vol20, p.ret1, p.ret5, p.ret20, p.pos20, p.prev_intraday, p.turnover_med, p.mkt_cap,
       p.short_interest, p.earn_yield, p.sector
FROM e JOIN read_parquet('{HIST}/quotes/*.parquet', union_by_name=true) q ON q.run_id = e.ranking_run_id AND q.symbol = e.symbol
JOIN p ON CAST(p.day AS DATE) = e.day AND p.symbol = e.symbol
"""


def pick(day, n):
    out, seen = [], set()
    for r in day.itertuples():
        if r.price * 100 > r.budget or (r.sector and r.sector in seen):
            continue
        seen.add(r.sector)
        out.append(r.Index)
        if len(out) == n:
            break
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-09-15", help="本発注は 2026-09-15 から")
    ap.add_argument("--n", type=int, default=4)
    ap.add_argument("--model", default="config/daytrade/models/lgbm_rank.txt")
    ap.add_argument("--cost-bp", type=float, default=43.7, help="揃えるコスト。evaluation の cost_bp は 2026-09-18 に 5.7 → 43.7 へ変わった")
    a = ap.parse_args()

    df = duckdb.sql(SQL, params=[a.since]).df()
    for c in ("price", "prev_close", "vol20", "turnover_med", "mkt_cap", "budget"):
        df[c] = df[c].astype(float)
    df["d"] = pd.to_datetime(df["d"])
    df["sector"] = df["sector"].fillna("")
    df["gap"] = df["price"] / df["prev_close"] - 1
    df["turn_cap"] = df["turnover_med"] / df["mkt_cap"].replace(0.0, np.nan)
    g = df[df["eligible"] & df["usable"] & (df["gap"] >= -1.0) & (df["gap"] < 0.0) & (df["price"] > limit_down(df["prev_close"].values))]
    g = with_rule_rank(g).reset_index(drop=True)
    g["lgbm"] = lgb.Booster(model_file=a.model).predict(ranked(raw_features(g), g["d"]).values)
    g["gap_vol"] = -g["key_sort"]

    rows = []
    for ranker in ("gap_vol", "lgbm"):
        t = g.sort_values(["d", ranker, "rule_rank"], ascending=[True, False, True], kind="mergesort")
        idx = [i for _, day in t.groupby("d", sort=False) for i in pick(day.head(60), a.n)]
        rows.append(t.loc[idx].assign(ranker=ranker))
    r = pd.concat(rows, ignore_index=True)
    r["pnl"] = r["even_pnl"].fillna(r["hypo_pnl"])  # even_pnl が無い日（〜9/17）は hypo_pnl
    qty = r["even_quantity"].where(r["even_pnl"].notna(), r["hypo_quantity"])
    r["pnl_adj"] = r["pnl"] - (a.cost_bp - r["cost_bp"]) / 1e4 * qty * r["open"]  # コストを全日で揃えた想定
    r["d"] = r["d"].dt.strftime("%m-%d")
    for c in ("gap", "gap_open", "ret_oc"):
        r[c] *= 100

    cols = ["d", "ranker", "code", "name", "rule_rank", "gap", "gap_open", "ret_oc", "net_bp", "pnl", "picked", "actual_pnl"]
    print(r[cols].round(2).to_string(index=False))
    print("\n## 日ごと（pnl は想定の円、bp は 4 銘柄の単純平均）")
    s = r.groupby(["d", "ranker"]).agg(pnl=("pnl", "sum"), pnl_adj=("pnl_adj", "sum"), bp=("net_bp", "mean"), same_as_live=("picked", "sum")).round(1).unstack("ranker")
    print(s.to_string())
    print("\n## 合計", r.groupby("ranker")["pnl"].sum().round().to_dict())
    print(f"## 合計（コストを {a.cost_bp} bp に揃える）", r.groupby("ranker")["pnl_adj"].sum().round().to_dict())


if __name__ == "__main__":
    main()
