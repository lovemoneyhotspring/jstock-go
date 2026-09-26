"""ロングが負ける相場環境で注文しない規則の検定（事前登録: vault 20-research/2026-09-jp-daytrade-long-regime-skip.md）。

  bash test/heavy.sh ./bin/daytrade backtest --config-dir test/out/cfg_gv --trades-csv test/out/gv_trades.csv
  bash test/heavy.sh ./bin/daytrade backtest --config-dir config/daytrade_margin --trades-csv test/out/mon_trades.csv
  PYTHONPATH=test test/.venv/bin/python test/dt_long_regime_skip.py

主は gap_vol の形、副は本番の形（LBZ2、インサンプル）。指標は前営業日の引けまで。IS の 3 分位の境目を OOS にそのまま使う。
"""
import numpy as np
import pandas as pd

from common import con

IS_END = pd.Timestamp("2021-12-31")
MAIN = "test/out/gv_trades.csv"
SUB = "test/out/mon_trades.csv"


def daily(path):
    t = pd.read_csv(path, parse_dates=["date"])
    return t.groupby("date").pnl.sum()


def market():
    c = con()
    tp = c.execute("SELECT Date d, C FROM topix WHERE C > 0 ORDER BY Date").df()
    tp["d"] = pd.to_datetime(tp.d).dt.tz_localize(None)
    s = tp.set_index("d").C
    r = s.pct_change()
    f = pd.DataFrame(index=s.index)
    f["topix_ret20"] = s / s.shift(20) - 1
    f["topix_ret60"] = s / s.shift(60) - 1
    f["topix_ma200"] = s / s.rolling(200).mean() - 1
    f["topix_vol20"] = r.rolling(20).std()
    f["topix_volratio"] = r.rolling(20).std() / r.rolling(250).std()
    disp = c.execute("""
        WITH x AS (
          SELECT Date d, Code, AdjC / LAG(AdjC) OVER (PARTITION BY Code ORDER BY Date) - 1 r, Va
          FROM bars WHERE AdjC > 0)
        SELECT d, STDDEV_SAMP(r) sd FROM x WHERE Va >= 1e8 AND r IS NOT NULL AND ABS(r) < 0.5 GROUP BY d ORDER BY d""").df()
    disp["d"] = pd.to_datetime(disp.d).dt.tz_localize(None)
    f["dispersion20"] = disp.set_index("d").sd.rolling(20).mean()
    # 前営業日の引けまで: 1 日ずらして当日の行に置く
    return f.shift(1)


def stats(p):
    eq = p.cumsum()
    sh = p.mean() / p.std() * np.sqrt(245) if p.std() > 0 else 0
    return p.sum(), sh, (eq - eq.cummax()).min()


def tstat(x):
    return x.mean() / (x.std() / np.sqrt(len(x))) if len(x) > 1 else np.nan


def run(p, f):
    f = f.reindex(p.index)
    f["own20"] = p.shift(1).rolling(20).sum()
    f["own60"] = p.shift(1).rolling(60).sum()
    isx, oos = p.index <= IS_END, p.index > IS_END
    rows = []
    for col in f.columns:
        q1, q2 = f.loc[isx, col].quantile([1 / 3, 2 / 3])
        for side, mask in (("下位", f[col] <= q1), ("上位", f[col] > q2)):
            mask = mask.fillna(False)
            is_skip, oos_skip = p[isx & mask], p[oos & mask]
            base = stats(p[oos])
            kept = stats(p[oos].where(~mask[oos], 0.0))
            rows.append(dict(
                指標=col, 休む=side, IS日数=len(is_skip), IS平均=is_skip.mean(), IS_t=tstat(is_skip),
                OOS日数=len(oos_skip), OOS平均=oos_skip.mean(), OOS_t=tstat(oos_skip),
                OOS損益差=kept[0] - base[0], OOSSharpe差=kept[1] - base[1], OOSDD差=kept[2] - base[2]))
    return pd.DataFrame(rows)


def main():
    f = market()
    pm, ps = daily(MAIN), daily(SUB)
    m, s = run(pm, f.copy()), run(ps, f.copy())
    m["副OOS平均"] = s["OOS平均"].values
    m["採用"] = ((m.IS平均 < 0) & (m.OOS平均 < 0) & (m.OOS_t <= -2.5) & (m.OOS損益差 > 0)
               & (m.OOSSharpe差 > 0) & (m.OOSDD差 >= 0) & (m.副OOS平均 < 0))
    pd.set_option("display.width", 250)
    print("主（gap_vol）の OOS:", "損益 {:,.0f}  Sharpe {:.2f}  DD {:,.0f}".format(*stats(pm[pm.index > IS_END])))
    print(m.round({"IS平均": 0, "IS_t": 2, "OOS平均": 0, "OOS_t": 2, "OOS損益差": 0, "OOSSharpe差": 2, "OOSDD差": 0, "副OOS平均": 0}).to_string(index=False))
    print("\n採用:", m[m.採用][["指標", "休む"]].values.tolist() or "なし")

    # 記述: 最大 DD の期間の環境（主）
    eq = pm.cumsum()
    dd = eq - eq.cummax()
    lo = dd.idxmin()
    hi = eq[:lo].idxmax()
    fx = f.reindex(pm.index)
    fx["own20"] = pm.shift(1).rolling(20).sum()
    fx["own60"] = pm.shift(1).rolling(60).sum()
    print(f"\n記述: 主の最大 DD {hi.date()}〜{lo.date()} の期間の指標の中央値（全期間の中央値）")
    w = fx.loc[hi:lo]
    print(pd.DataFrame({"DD 期間": w.median(), "全期間": fx.median()}).round(4).to_string())


if __name__ == "__main__":
    main()
