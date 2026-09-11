# 段階 6: 現行の選定規則（gap/vol20 の小さい順に 3 銘柄）を決定時刻ごとに回す。
#   待つ   … T までに寄っている銘柄だけから選び、T の値段で買う（実際にできること）
#   成行   … T までに寄っていない銘柄も選べ、寄値で約定する（backtest の前提。
#            9:01 に成行を出せば板寄せに参加するので、これも実際にできる。ただし
#            **選ぶ時点で寄値は分からない**）
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
days = df.Date.nunique()

def run(label, price_col, wait):
    rows = []
    for _, d in df.groupby('Date'):
        pool = d[d.open_time <= T] if wait else d
        pool = pool.dropna(subset=[price_col])
        if pool.empty: continue
        pick = pool.nsmallest(N, 'k')
        r = (pick.close_raw / pick[price_col] - 1) * 1e4 - COST
        rows.append(dict(date=_, n=len(pick), bp=r.mean(), gap=pick.gap_pct.mean(),
                         pnl=(r / 1e4 * BUDGET).sum()))
    s = pd.DataFrame(rows)
    print(f"  {label:<26} 銘柄/日 {s.n.mean():.1f}  平均ギャップ {s.gap.mean():+5.2f}%  "
          f"net {s.bp.mean():+6.1f} bp  勝ち日 {100*(s.pnl>0).mean():4.1f}%  "
          f"2 年の損益 {s.pnl.sum():>11,.0f} 円")

print(f"495 営業日。1 銘柄 100 万円 × 3 銘柄、往復 {COST} bp 込み\n")
for T in TIMES:
    col = 'px_' + T.replace(':', '')
    print(f"― 決定時刻 {T}")
    run('待つ（T の値段で買う）', col, True)
T = '09:01'
run('成行（寄値で約定・上限）', 'open_px', False)
