# 段階 4: ギャップ帯 × 寄り付き時刻の分布と、帯ごとの寄→引。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import numpy as np

c = con()
df = c.execute(f"SELECT * FROM read_parquet('{OUT}/open_gap.parquet')").df()
df['gap_pct'] = df.gap * 100
BANDS = [-100, -5, -4, -3, -2, -1.5, -1, -0.5, 0]
LAB = ['≤−5%', '−5〜−4', '−4〜−3', '−3〜−2', '−2〜−1.5', '−1.5〜−1', '−1〜−0.5', '−0.5〜0']
g = df[df.gap_pct < 0].copy()
g['帯'] = pd.cut(g.gap_pct, BANDS, labels=LAB)
g['t'] = g.open_time

print(f"対象 {len(g):,} 件（ギャップ < 0 のみ、495 営業日、プライム・売買代金 1 億円以上・時価総額の下位 1/3 を除く）\n")
print("■ ギャップ帯ごとに「その時刻までに寄っている」割合")
rows = []
for lab, sub in g.groupby('帯', observed=True):
    r = {'帯': lab, '件数': len(sub), '1 日あたり': round(len(sub) / g.Date.nunique(), 1)}
    for t in ['09:00', '09:01', '09:02', '09:03', '09:05', '09:07', '09:10', '09:15']:
        r[t] = round(100 * (sub.t <= t).mean(), 1)
    r['寄→引 bp'] = round(sub.oc.mean() * 1e4, 1)
    rows.append(r)
print(pd.DataFrame(rows).to_string(index=False))

print("\n■ 寄り付き時刻ごとの寄→引（その時刻に寄った銘柄の当日の寄→引 bp）")
g['t帯'] = pd.cut(g.t.str.replace(':', '').astype(int),
                 [855, 900, 901, 903, 905, 910, 915, 930, 2400],
                 labels=['09:00', '09:01', '09:02-03', '09:04-05', '09:06-10', '09:11-15', '09:16-30', '09:31 以降'])
p = g.groupby('t帯', observed=True).agg(件数=('oc', 'size'), oc_bp=('oc', lambda s: round(s.mean() * 1e4, 1)),
                                       gap=('gap_pct', lambda s: round(s.mean(), 2)))
p['割合'] = (100 * p.件数 / len(g)).round(1)
print(p.to_string())
