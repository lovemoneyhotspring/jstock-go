# 段階 9: 決定時刻 × ギャップの足切りの格子。9:0T に成行を出す（未寄付は板寄せの寄値、
# 寄済みは T の値段で約定）。決算の前日・当日は除外（config と同じ）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

TIMES = ['09:01', '09:03', '09:05', '09:07', '09:10', '09:15']
CUTS = [0.0, -2.0, -3.0, -4.0, -5.0]
COST, N, BUDGET = 5.7, 3, 1_000_000
c = con()
df = c.execute(f"""
 WITH e AS (SELECT DISTINCT Code, CAST(DiscDate AS DATE) d FROM fins),
 cal AS (SELECT Date, LAG(Date) OVER (ORDER BY Date) prev FROM (SELECT DISTINCT Date FROM bars))
 SELECT g.Date, g.Code, g.gap*100 gap_pct, g.open_time, g.vol20, g.open_px, b.C close_raw,
        {', '.join('p.px_' + t.replace(':','') for t in TIMES)}
 FROM read_parquet('{OUT}/open_gap.parquet') g
 JOIN read_parquet('{OUT}/open_px_at.parquet') p ON p.Date=g.Date AND p.Code=g.Code
 JOIN bars b ON b.Date=g.Date AND b.Code=g.Code
 JOIN cal ON cal.Date=g.Date
 WHERE g.gap<0 AND b.C>0 AND g.open_px>0
   AND NOT EXISTS (SELECT 1 FROM e WHERE e.Code=g.Code AND e.d=CAST(g.Date AS DATE))
   AND NOT EXISTS (SELECT 1 FROM e WHERE e.Code=g.Code AND e.d=CAST(cal.prev AS DATE))""").df()
df['k'] = (df.gap_pct / 100) / df.vol20

bp, pnl, cnt = {}, {}, {}
for T in TIMES:
    col = 'px_' + T.replace(':', '')
    d0 = df.assign(fill=np.where(df.open_time > T, df.open_px, df[col])).dropna(subset=['fill'])
    for cut in CUTS:
        rows = []
        for day, d in d0[d0.gap_pct <= cut].groupby('Date'):
            pick = d.nsmallest(N, 'k')
            r = (pick.close_raw / pick.fill - 1) * 1e4 - COST
            rows.append(dict(n=len(pick), bp=r.mean(), pnl=(r / 1e4 * BUDGET).sum()))
        s = pd.DataFrame(rows)
        lab = 'なし' if cut == 0 else f'≤{cut:.0f}%'
        bp.setdefault(lab, {})[T] = round(s.bp.mean(), 1)
        pnl.setdefault(lab, {})[T] = int(s.pnl.sum())
        cnt.setdefault(lab, {})[T] = f"{len(s)}日/{s.n.mean():.1f}"
print(f"495 営業日・決算除外・1 銘柄 100 万円 × 最大 {N}・往復 {COST} bp 込み。行 = ギャップの足切り\n")
print("■ net bp"); print(pd.DataFrame(bp).T.to_string())
print("\n■ 2 年の損益（円）"); print(pd.DataFrame(pnl).T.map(lambda v: f"{v:,}").to_string())
print("\n■ 建てた日数 / 1 日あたり銘柄数"); print(pd.DataFrame(cnt).T.to_string())
