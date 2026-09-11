# 段階 8: 9:01 に成行を出す（板寄せに参加して寄値で約定する）前提で、**選び方**を比べる。
# 9:01 時点で未寄付の銘柄は、その事実だけで「ギャップが深い」を意味する（≤−3% の銘柄は
# 9:00 にほぼ 100% が未寄付）。順位付けに寄値は使えないので、使える情報だけで比べる。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import numpy as np

COST, N, BUDGET = 5.7, 3, 1_000_000
rng = np.random.default_rng(20260911)
c = con()
df = c.execute(f"""
  SELECT g.Date, g.Code, g.gap*100 AS gap_pct, g.open_time, g.vol20, g.open_px, b.C AS close_raw
  FROM read_parquet('{OUT}/open_gap.parquet') g
  JOIN bars b ON b.Date = g.Date AND b.Code = g.Code
  WHERE g.gap < 0 AND b.C > 0 AND g.open_px > 0""").df()
df['k'] = (df.gap_pct / 100) / df.vol20
df['unopened_0901'] = df.open_time > '09:01'

def sim(label, pool_fn, order):
    rows = []
    for day, d in df.groupby('Date'):
        pool = pool_fn(d)
        if pool.empty: continue
        pick = order(pool).head(N)
        r = (pick.close_raw / pick.open_px - 1) * 1e4 - COST
        rows.append(dict(n=len(pick), bp=r.mean(), gap=pick.gap_pct.mean(), pnl=(r / 1e4 * BUDGET).sum()))
    s = pd.DataFrame(rows)
    print(f"  {label:<34} 建てた日 {len(s):>3}  銘柄/日 {s.n.mean():.1f}  ギャップ {s.gap.mean():+6.2f}%  "
          f"net {s.bp.mean():+6.1f} bp  2 年 {s.pnl.sum():>10,.0f} 円")

deep = lambda p: p.sort_values('k')
rand = lambda p: p.sample(frac=1, random_state=rng.integers(1 << 30))
lowvol = lambda p: p.sort_values('vol20')

print(f"495 営業日。9:01 に成行 → 寄値で約定。1 銘柄 100 万円 × {N}、往復 {COST} bp 込み\n")
print("【上限】寄値を知っていれば")
sim('全母集団から深い順（backtest の前提）', lambda d: d, deep)
print("\n【現行】9:01 に寄っている銘柄からしか選べない")
sim('寄済みから深い順', lambda d: d[~d.unopened_0901], deep)
print("\n【提案】未寄付（＝深いギャップ）を優先する")
sim('未寄付から深い順（順位が完璧なら）', lambda d: d[d.unopened_0901], deep)
sim('未寄付から無作為（順位を捨てる）', lambda d: d[d.unopened_0901], rand)
sim('未寄付からボラの小さい順', lambda d: d[d.unopened_0901], lowvol)
