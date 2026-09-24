"""当日の場中に、その朝の気配で lgbm / gap_vol なら何を建てたかを並べる（日足が無くても動く）。

test/dt_live_shadow.py の選定部分だけ。採点（日足）は引け後に dt_live_shadow.py で行う。
値段は立花の時価（bin/daytrade quotes）で当てる。

  test/.venv/bin/python test/dt_live_shadow_now.py [--day 2026-09-24]
"""

import argparse

import duckdb
import lightgbm as lgb
import numpy as np
import pandas as pd

from dt_candidates import limit_down
from dt_lgbm_train import ranked, raw_features
from dt_live_shadow import HIST
from dt_preopen_sim import with_rule_rank
import dt_wf_target
from dt_wf_target import evaluate

SQL = f"""
WITH q0 AS (
  SELECT *, min(recorded_at) OVER (PARTITION BY day, run_id) run_at
  FROM read_parquet('{HIST}/quotes/*.parquet', union_by_name=true) WHERE CAST(day AS DATE) = CAST(? AS DATE)
    AND CAST(timezone('Asia/Tokyo', recorded_at) AS DATE) = CAST(day AS DATE)
    AND hour(timezone('Asia/Tokyo', recorded_at)) >= 8),
q AS (SELECT * FROM q0 QUALIFY run_at = min(run_at) OVER (PARTITION BY day)),
p AS (
  SELECT * FROM read_parquet('{HIST}/plan/*.parquet', union_by_name=true) WHERE CAST(day AS DATE) = CAST(? AS DATE)
  QUALIFY recorded_at = max(recorded_at) OVER (PARTITION BY day))
SELECT CAST(q.day AS DATE) d, q.symbol code, q.run_at, q.price, q.prev_close, q.gap, q.from_book,
       p.vol20, p.ret1, p.ret5, p.ret20, p.pos20, p.prev_intraday, p.turnover_med, p.mkt_cap,
       p.short_interest, p.earn_yield, p.sector
FROM q JOIN p ON p.symbol = q.symbol
WHERE p.eligible AND q.usable AND q.price > 0 AND q.prev_close > 0
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--day", default=pd.Timestamp.now(tz="Asia/Tokyo").strftime("%Y-%m-%d"))
    ap.add_argument("--model", default="config/daytrade/models/lgbm_rank.txt")
    ap.add_argument("--n", type=int, default=dt_wf_target.N, help="銘柄数（本番のロングは 500 万で N=4）")
    a = ap.parse_args()
    dt_wf_target.N = a.n

    df = duckdb.sql(SQL, params=[a.day, a.day]).df()
    for c in ("price", "prev_close", "gap", "vol20", "turnover_med", "mkt_cap"):
        df[c] = df[c].astype(float)
    df["d"] = pd.to_datetime(df["d"])
    df["sector"] = df["sector"].fillna("")
    df["gap"] = df["price"] / df["prev_close"] - 1
    df["y_raw"] = np.nan
    df["turn_cap"] = df["turnover_med"] / df["mkt_cap"].replace(0.0, np.nan)
    g = df[(df["gap"] >= -1.0) & (df["gap"] < 0.0) & (df["price"] > limit_down(df["prev_close"].values))]
    g = with_rule_rank(g).reset_index(drop=True)
    score = lgb.Booster(model_file=a.model).predict(ranked(raw_features(g), g["d"]).values)
    round_at = pd.to_datetime(g["run_at"].iloc[0], utc=True).tz_convert("Asia/Tokyo").strftime("%H:%M:%S")
    print(f"{a.day} の最初の回（{round_at}）の気配、候補 {len(g)} 銘柄")
    for ranker, s in (("gap_vol", -g["key_sort"].values), ("lgbm", score)):
        p = evaluate(g, s, ranker, 0).merge(g[["code", "prev_close", "sector"]], on="code")
        for _, r in p.iterrows():
            print(f"{ranker},{r['code']},{r['w']:.4f},{r['gap'] * 100:.2f},{r['prev_close']:.0f},{r['rule_rank']},{r['sector']}")


if __name__ == "__main__":
    main()
