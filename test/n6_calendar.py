# N6. カレンダー・アノマリー。月内の営業日位置（先頭から / 末尾から）と曜日で TOPIX の日次を割る。
# 実需（投信の設定解約・パッシブの月末リバランス）が機械的な注文を出すなら、ここに偏りが出るはず。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con(); tp = topix(c); tdays = tp.index
o, cl = tp.open.values, tp.close.values
df = pd.DataFrame({'o': o, 'c': cl}, index=tdays)
df['intraday'] = df.c / df.o - 1                       # 寄り → 引け
df['overnight'] = df.o / df.c.shift(1) - 1             # 前引け → 寄り
df['daily'] = df.c / df.c.shift(1) - 1
per = tdays.to_period('M')
df['fwd'] = df.groupby(per).cumcount() + 1                                  # 月初から何営業日目
df['bwd'] = df.groupby(per).cumcount(ascending=False) + 1                   # 月末から何営業日目
df['dow'] = tdays.dayofweek
def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan

def table(key, vals, col):
    print(f'\n=== {key}（TOPIX の {col}、bp）===')
    print(f'{"区分":>6} {"日数":>5} {"全期間":>8} {"t":>6} {"IS":>8} {"t":>6} {"OOS":>8} {"t":>6}')
    for v in vals:
        s = df.loc[df[key] == v, col].dropna()
        if len(s) < 30: continue
        i, o_ = s[s.index <= IS_END], s[s.index > IS_END]
        print(f'{v:>6} {len(s):>5} {s.mean()*1e4:>8.1f} {tv(s):>6.2f} {i.mean()*1e4:>8.1f} {tv(i):>6.2f} '
              f'{o_.mean()*1e4:>8.1f} {tv(o_):>6.2f}')
    s = df[col].dropna()
    print(f'{"全体":>6} {len(s):>5} {s.mean()*1e4:>8.1f} {tv(s):>6.2f}')

for col in ('daily', 'intraday', 'overnight'):
    table('bwd', range(1, 6), col)
    table('fwd', range(1, 6), col)
table('dow', range(5), 'daily')

# --- 月末 → 月初の持ち越し（月末 k 日前の引けで買い、月初 j 日目の引けで売る）---
print('\n=== 月末 k 日前の引け → 月初 j 日目の引け（TOPIX 買い持ち、費用 4bp/往復）===')
print(f'{"k":>3} {"j":>3} {"回数":>5} {"純bp":>8} {"t":>6} {"IS":>8} {"OOS":>8} {"年率%":>7}')
close = df.c
for k in range(1, 6):
    for j in range(1, 6):
        buys = df.index[df.bwd == k]; sells = df.index[df.fwd == j]
        r = []
        for b in buys:
            nxt = sells[sells > b]
            if len(nxt) == 0: continue
            r.append((b, close[nxt[0]] / close[b] - 1))
        if len(r) < 30: continue
        s = pd.Series([x[1] for x in r], index=[x[0] for x in r])
        net = s * 1e4 - 4
        i, o_ = net[net.index <= IS_END], net[net.index > IS_END]
        print(f'{k:>3} {j:>3} {len(net):>5} {net.mean():>8.1f} {tv(net):>6.2f} {i.mean():>8.1f} {o_.mean():>8.1f} '
              f'{net.mean()*12/100:>7.2f}')
