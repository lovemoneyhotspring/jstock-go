# 段階 7: 「寄ったばかりの銘柄だけ買う」。決定時刻 T で、直前 W 分以内に寄った銘柄に限る。
# 早く寄った銘柄を T で買うと最初の数分の反発を払う（2026-09-jp-gap-minute の発見 1）。
# 寄り立ての銘柄なら T の値段 ≒ 寄値なので、反発を払わずに深いギャップだけ拾える。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

TIMES = ['09:01', '09:03', '09:05', '09:07', '09:10', '09:15']
COST, N, BUDGET = 5.7, 3, 1_000_000
c = con()
df = c.execute(f"""
  SELECT g.Date, g.Code, g.gap*100 AS gap_pct, g.open_time, g.vol20, g.open_px,
         b.C AS close_raw, {', '.join('p.px_' + t.replace(':','') for t in TIMES)}
  FROM read_parquet('{OUT}/open_gap.parquet') g
  JOIN read_parquet('{OUT}/open_px_at.parquet') p ON p.Date = g.Date AND p.Code = g.Code
  JOIN bars b ON b.Date = g.Date AND b.Code = g.Code
  WHERE g.gap < 0 AND b.C > 0""").df()
df['k'] = (df.gap_pct / 100) / df.vol20
df['tmin'] = df.open_time.str[:2].astype(int) * 60 + df.open_time.str[3:5].astype(int)

def sim(pool_fn, price, label):
    rows = []
    for day, d in df.groupby('Date'):
        pool = pool_fn(d).dropna(subset=[price])
        if pool.empty: continue
        pick = pool.nsmallest(N, 'k')
        r = (pick.close_raw / pick[price] - 1) * 1e4 - COST
        rows.append(dict(n=len(pick), bp=r.mean(), gap=pick.gap_pct.mean(), pnl=(r / 1e4 * BUDGET).sum()))
    s = pd.DataFrame(rows)
    print(f"  {label:<30} 建てた日 {len(s):>3}  銘柄/日 {s.n.mean():.1f}  ギャップ {s.gap.mean():+5.2f}%  "
          f"net {s.bp.mean():+6.1f} bp  2 年 {s.pnl.sum():>10,.0f} 円")

print(f"495 営業日。1 銘柄 100 万円 × 最大 {N} 銘柄、往復 {COST} bp 込み\n")
for T in TIMES:
    col, tm = 'px_' + T.replace(':', ''), int(T[:2]) * 60 + int(T[3:5])
    print(f"― 決定時刻 {T}")
    sim(lambda d: d[d.tmin <= tm], col, 'その時刻までに寄った全部')
    for W in (2, 5):
        sim(lambda d, w=W, t=tm: d[(d.tmin <= t) & (d.tmin > t - w)], col, f'直前 {W} 分以内に寄った銘柄だけ')
