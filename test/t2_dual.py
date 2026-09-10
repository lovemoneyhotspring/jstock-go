# T2. 指数のデュアルモメンタム。毎月末に L か月リターンの高いほう（^N225 / ^GSPC）を翌月持つ。どちらもゼロ以下なら現金（^IRX）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import itertools

COST = 0.0002
rf = irx()

def monthly(close):
    return close.groupby(close.index.to_period('M')).last()

def run(pair, L, use_cash, is_end):
    names = list(pair)
    m = pd.concat([monthly(pair[n]) for n in names], axis=1, keys=names).dropna()
    mret = m.pct_change()
    mom = (m / m.shift(L) - 1)
    rfm = monthly(rf).reindex(m.index).ffill().fillna(0) / 12
    mom = mom.dropna()
    pick = mom.idxmax(axis=1)
    best = mom.max(axis=1)
    if use_cash:
        pick = pick.where(best > 0, 'CASH')
    pick = pick.reindex(m.index).shift(1)   # 月末の判断で翌月を持つ
    r = pd.Series(np.nan, index=m.index)
    for n in names:
        r[pick == n] = mret[n][pick == n]
    r[pick == 'CASH'] = rfm[pick == 'CASH']
    switch = (pick != pick.shift(1)) & pick.notna() & pick.shift(1).notna()
    r = r - switch * 2 * COST
    r = r.dropna()
    idx = r.index.to_timestamp(how='end').normalize()
    r.index = idx
    a, b = r[idx <= is_end], r[idx > is_end]
    return a, b, pick

def mstats(r):  # 月次
    return stats(r, n=12)

rows = []
for label, pair, is_end in [
    ('^N225 / ^GSPC 30年', {'N225': wb('^N225').close, 'GSPC': wb('^GSPC').close}, LONG_IS_END),
    ('1306 / SPY 2016〜', {'1306': jp_etf(con(), '13060').close, 'SPY': wb('SPY').close}, IS_END),
]:
    print(f"\n== {label}")
    names = list(pair)
    m = pd.concat([monthly(pair[n]) for n in names], axis=1, keys=names).dropna()
    m.index = m.index.to_timestamp(how='end').normalize()
    for n in names:
        r = m[n].pct_change().dropna(); r = r[r.index >= m.index[13]]
        a, b = r[r.index <= is_end], r[r.index > is_end]
        print(f"  {n+' 買い持ち':16s} IS {fmt(mstats(a))} | OOS {fmt(mstats(b))}")
        for per, x in [('IS', a), ('OOS', b)]: rows.append(dict(series=label, strategy=f'{n} 買い持ち', period=per, **mstats(x)))
    r = (m.pct_change().mean(axis=1)).dropna(); r = r[r.index >= m.index[13]]
    a, b = r[r.index <= is_end], r[r.index > is_end]
    print(f"  {'50/50 月次':16s} IS {fmt(mstats(a))} | OOS {fmt(mstats(b))}")
    for per, x in [('IS', a), ('OOS', b)]: rows.append(dict(series=label, strategy='50/50', period=per, **mstats(x)))
    grid = []
    for L, cash in itertools.product([6, 9, 12], [True, False]):
        a, b, pick = run(pair, L, cash, is_end)
        a = a[a.index >= m.index[13]]
        sa, sb = mstats(a), mstats(b)
        lab = f"L{L} {'現金あり' if cash else '常時保有'}"
        share = pick.value_counts(normalize=True).round(2).to_dict()
        print(f"  {lab:16s} IS {fmt(sa)} | OOS {fmt(sb)}  配分 {share}")
        grid.append(dict(label=lab, is_cagr=sa['cagr'], is_dd=sa['dd'], oos_cagr=sb['cagr'], oos_dd=sb['dd']))
        for per, x in [('IS', sa), ('OOS', sb)]: rows.append(dict(series=label, strategy=lab, period=per, **x))
    g = pd.DataFrame(grid)
    print(f"  格子 6 の中央値: IS CAGR {g.is_cagr.median():.2f}% DD {g.is_dd.median():.1f}% | OOS CAGR {g.oos_cagr.median():.2f}% DD {g.oos_dd.median():.1f}%")
pd.DataFrame(rows).to_csv(f'{OUT}/t2_dual.csv', index=False)
print('saved')
