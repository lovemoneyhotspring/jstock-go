"""ショートを口座全体（ロング ＋ h × ショート）の Sharpe と最大 DD で測る
（事前登録: vault 20-research/2026-09-jp-daytrade-short-account-dd.md）。

設定（test/out/ は git の管理外なので、ここに書いておく）:
  test/out/cfg_gv/daytrade.toml       extends = "../../../config/daytrade_margin"
                                      [signal] rank_by = "gap_vol" / rank_by_us_low = "gap_vol"  [regime] us_skip_legs = "all"
  test/out/cfg_gv_s200/daytrade.toml  extends = "../cfg_gv"  [margin] paused = false
  test/out/cfg_gv_s100/daytrade.toml  extends = "../cfg_gv"  [margin] paused = false / max_capital = 1000000
各設定を bash test/heavy.sh ./bin/daytrade backtest --config-dir <dir> --trades-csv test/out/<name>_trades.csv で回してから:
  test/.venv/bin/python test/dt_short_account_dd.py
事後の探索（ショートも寄成）: cfg_gv_s200p / cfg_gv_s100p = extends "../cfg_gv_s200" / "../cfg_gv_s100" に [execution] preopen_legs = "both"
  test/.venv/bin/python test/dt_short_account_dd.py L=test/out/gv_trades.csv LS200p=test/out/gv_s200p_trades.csv LS100p=test/out/gv_s100p_trades.csv
"""
import sys

import numpy as np
import pandas as pd

IS_END = pd.Timestamp("2021-12-31")
RUNS = {"L": "test/out/gv_trades.csv", "LS200": "test/out/gv_s200_trades.csv", "LS100": "test/out/gv_s100_trades.csv"}
if len(sys.argv) > 1:  # 事後の探索用に別の組を渡す: name=path ...
    RUNS = dict(a.split("=", 1) for a in sys.argv[1:])


def legs(path, idx):
    t = pd.read_csv(path, parse_dates=["date"])
    g = t.groupby(["date", "side"]).pnl.sum().unstack().fillna(0.0)
    g = g.reindex(idx, fill_value=0.0)
    return g.get("long", pd.Series(0.0, idx)), g.get("short", pd.Series(0.0, idx))


def stats(p):
    eq = p.cumsum()
    return p.sum(), p.mean() / p.std() * np.sqrt(245), (eq - eq.cummax()).min()


def main():
    idx = sorted(set().union(*[set(pd.read_csv(p, parse_dates=["date"]).date) for p in RUNS.values()]))
    idx = pd.DatetimeIndex(idx)
    rows = []
    base = {}
    for name, path in RUNS.items():
        lg, sh = legs(path, idx)
        for h in ([1.0] if name == "L" else [0.65, 0.5, 1.0, 0.0]):
            p = lg + h * sh
            for per, m in (("IS", idx <= IS_END), ("OOS", idx > IS_END)):
                s = stats(p[m])
                rows.append(dict(形=name, h=h, 期間=per, 損益=s[0], Sharpe=s[1], 最大DD=s[2]))
        if name != "L":
            act = (lg != 0) | (sh != 0)
            worst = lg[act].nsmallest(20).index
            print(f"{name}: ロングとショートの日次の相関 {lg[act].corr(sh[act]):.3f}（両方建てた日 {((lg != 0) & (sh != 0)).sum()}）"
                  f"  ロングの最悪 20 日のロング {lg[worst].sum():,.0f} / ショート {sh[worst].sum():,.0f}  ショート単独 {sh.sum():,.0f}")
    r = pd.DataFrame(rows)
    pd.set_option("display.width", 200)
    print(r.round({"損益": 0, "Sharpe": 2, "最大DD": 0}).to_string(index=False))

    L = r[r.形 == "L"].set_index("期間")
    for name in [n for n in RUNS if n != "L"]:
        x = r[(r.形 == name) & (r.h == 0.65)].set_index("期間")
        y = r[(r.形 == name) & (r.h == 0.5)].set_index("期間")
        ok = all(x.Sharpe[p] > L.Sharpe[p] and x.最大DD[p] >= L.最大DD[p] for p in ("IS", "OOS")) and y.最大DD["OOS"] >= L.最大DD["OOS"]
        print(f"採用 {name}: {'○' if ok else '×'}")


if __name__ == "__main__":
    main()
