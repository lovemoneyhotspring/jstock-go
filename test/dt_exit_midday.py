"""ロングの出口 前引け / 15:20 / 大引け を日ごとの損益の差で比べる（2026-09-24）。

入力は test/dt_exit_midday.sh の test/out/dt_exit_{1130,1520,close}.csv。
日単位に畳んでから差の t を取る（シグナル単位だと t が膨らむ）。

    test/.venv/bin/python test/dt_exit_midday.py
"""
import math

import pandas as pd

OUT = "test/out"
names = {"1130": "前引け 11:30", "1520": "15:20 成行", "close": "大引け 15:30"}
tr = {k: pd.read_csv(f"{OUT}/dt_exit_{k}.csv", dtype={"code": str}) for k in names}
day = {k: t.groupby("date")["pnl"].sum() for k, t in tr.items()}
bp = {k: (t["pnl"].sum() / t["amount"].sum() * 1e4) for k, t in tr.items()}


def stats(s):
    m, sd, n = s.mean(), s.std(ddof=1), len(s)
    return m, m / (sd / math.sqrt(n)), n


print("出口          合計損益(円)   1 取引の平均(bp)  日次の平均(円)")
for k, label in names.items():
    print(f"{label:12s} {day[k].sum():14,.0f} {bp[k]:12.1f} {day[k].mean():14,.0f}")

print("\n日ごとの差（左 − 右）")
for a, b in [("1130", "close"), ("1130", "1520"), ("close", "1520")]:
    d = (day[a] - day[b]).dropna()
    m, t, n = stats(d)
    print(f"{names[a]} − {names[b]}: 平均 {m:+,.0f} 円/日  t {t:+.2f}  日数 {n}  "
          f"左が勝った日 {(d > 0).mean():.0%}")

print("\n年別の合計（万円）")
yr = pd.DataFrame({names[k]: v.groupby(v.index.str[:4]).sum() / 1e4 for k, v in day.items()})
print(yr.round(1).to_string())

# 前引けより大引けが良い日・悪い日の偏り（ギャップの深さ別。1 取引の bp）
m = tr["1130"].merge(tr["close"], on=["date", "code"], suffixes=("_mid", "_cl"))
m["diff_bp"] = (m["exit_mid"] - m["exit_cl"]) / m["entry_mid"] * 1e4
m["gap_q"] = pd.qcut(m["gap_mid"], 4, labels=["深い", "2", "3", "浅い"])
print("\n前引け − 大引け（1 取引の bp、ギャップの深さ 4 分位）")
print(m.groupby("gap_q", observed=True)["diff_bp"].agg(["mean", "count"]).round(1).to_string())
print(m.groupby("rank_mid")["diff_bp"].agg(["mean", "count"]).round(1).to_string())

# 11:30 の板寄せで約定が無かった銘柄は、分足の約定が 12:30 以降の最初の値になる（約 8%）。
# それを除いても向きが同じかを見る（test/out/dt_exit_has1130.csv は jquants query で作る。手順はノート）
try:
    has = pd.read_csv(f"{OUT}/dt_exit_has1130.csv", dtype={"code": str}).assign(ok=True)
    has["d"] = has["d"].astype(str)
    m = m.merge(has, left_on=["date", "code"], right_on=["d", "code"], how="left")
    m["ok"] = m["ok"].fillna(False).astype(bool)
    print("\n11:30 に約定があったか別の 前引け − 大引け（bp）")
    print(m.groupby("ok")["diff_bp"].agg(["mean", "count"]).round(1).to_string())
except FileNotFoundError:
    pass
