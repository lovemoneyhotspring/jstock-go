"""本番の形（config/daytrade_margin）の backtest の取引 CSV から、損の大きさの端を出す。

  bash test/heavy.sh ./bin/daytrade backtest --config-dir config/daytrade_margin --trades-csv test/out/mon_trades.csv
  test/.venv/bin/python test/dt_monday_risk.py [test/out/mon_trades.csv]

最悪の日・最悪の月・最大 DD の山と谷と回復・持ち越し（ストップ安で引けて返済できなかった分）・倍率 > 1 の日を出す。
"""
import sys

import pandas as pd

path = sys.argv[1] if len(sys.argv) > 1 else "test/out/mon_trades.csv"
t = pd.read_csv(path, parse_dates=["date"])
d = t.groupby("date").agg(pnl=("pnl", "sum"), amt=("amount", "sum"), n=("code", "count"), scale=("scale", "max"))
eq = d.pnl.cumsum()
dd = eq - eq.cummax()

print("最悪の日 10")
print(d.sort_values("pnl").head(10).to_string())
m = d.pnl.resample("ME").sum()
print("\n最悪の月 5")
print(m.sort_values().head(5).to_string())
print("負けた月", (m < 0).sum(), "/", (m != 0).sum())

i = dd.idxmin()
pk = eq[:i].idxmax()
rec = eq[i:][eq[i:] >= eq[pk]]
print(f"\n最大 DD {dd.min():,.0f}  山 {pk.date()}  谷 {i.date()}  回復 {rec.index[0].date() if len(rec) else '未'}")
for y in sorted(set(d.index.year)):
    x = d[d.index.year == y].pnl.cumsum()
    print(f"  {y} 年内の DD {(x - x.cummax()).min():>12,.0f}")

print(f"\n1 日の建玉 中央 {d.amt.median():,.0f}  最大 {d.amt.max():,.0f}")
c = t[t.carried]
print(f"持ち越し {len(c)} / {len(t)} 件（{len(c) / len(t):.2%}）  損益の合計 {c.pnl.sum():,.0f}  最悪 {c.pnl.min():,.0f}")
print(c.sort_values("pnl").head(10)[["date", "code", "gap", "shares", "entry", "exit", "pnl"]].to_string())
s = d[d.scale > 1]
print(f"\n倍率 > 1 の日 {len(s)}  損益 {s.pnl.sum():,.0f}  最悪 {s.pnl.min():,.0f}  平均 {s.pnl.mean():,.0f}（それ以外 {d[d.scale <= 1].pnl.mean():,.0f}）")
