"""本番の形（規則 R・最大 10 本）で、選定の順位ごとの効き（bp/取引、コスト後）を出す。

  bash test/heavy.sh ./bin/daytrade backtest --config-dir config/daytrade_margin --trades-csv test/out/mon_trades.csv   # LBZ2（全期間で学習＝インサンプル）
  bash test/heavy.sh ./bin/daytrade backtest --config-dir test/out/cfg_gv --trades-csv test/out/gv_trades.csv          # gap_vol（学習なし）
  test/.venv/bin/python test/dt_rank_cliff_now.py
"""
import numpy as np
import pandas as pd

for name, path in (("LBZ2（インサンプル）", "test/out/mon_trades.csv"), ("gap_vol", "test/out/gv_trades.csv")):
    t = pd.read_csv(path, parse_dates=["date"])
    t = t[t.side == "long"].copy()
    t["bp"] = t.pnl / t.amount * 1e4
    t["期"] = np.where(t.date.dt.year <= 2021, "2017-21", "2022-26")
    g = t.groupby(["rank", "期"]).bp.agg(["mean", "std", "count"]).unstack("期")
    out = pd.DataFrame({
        "件数": t.groupby("rank").size(),
        "bp": t.groupby("rank").bp.mean(),
        "t": t.groupby("rank").bp.apply(lambda x: x.mean() / x.std() * np.sqrt(len(x))),
        "2017-21": g[("mean", "2017-21")], "2022-26": g[("mean", "2022-26")],
        "金額の比率": t.groupby("rank").amount.sum() / t.amount.sum(),
    })
    print(f"== {name}")
    print(out.round({"bp": 1, "t": 2, "2017-21": 1, "2022-26": 1, "金額の比率": 3}).to_string())
