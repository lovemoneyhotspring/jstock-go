# 案 1. 決算の反応日の寄り買い引け売り。反応日のギャップ（AdjO/前日 AdjC-1）で上下に分け、横断の bp/t と N=3 の籠。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
c = con()
tp = topix(c); tdays = tp.index; tidx = pd.Series(np.arange(len(tdays)), index=tdays)
ev = c.execute("""
SELECT Code, DiscDate, DiscTime FROM fins
WHERE DocType LIKE '%FinancialStatements%' AND DocType NOT LIKE '%REIT%' AND DocType NOT LIKE '%Foreign%' AND DocType NOT LIKE 'OtherPeriod%'
""").df()
ev['DiscDate'] = pd.to_datetime(ev.DiscDate)
ev['t'] = ev.DiscTime.fillna('').str.slice(0, 5)
close_t = np.where(ev.DiscDate >= '2024-11-05', '15:30', '15:00')
ev['after'] = (ev.t >= close_t) | (ev.t == '')
pos = np.searchsorted(tdays.values, ev.DiscDate.values, side='left')
same = (pos < len(tdays)) & (tdays.values[np.minimum(pos, len(tdays)-1)] == ev.DiscDate.values)
ev['rpos'] = np.where(same & ~ev.after.values, pos, pos + np.where(same, 1, 0))
ev = ev[(ev.rpos >= 1) & (ev.rpos < len(tdays))].copy()
ev['rdate'] = tdays.values[ev.rpos.values]
ev = ev.sort_values(['Code', 'rdate', 'DiscDate']).drop_duplicates(['Code', 'rdate'])
codes = tuple(ev.Code.unique())
px = c.execute(f"SELECT Code, Date, AdjO, AdjC, O, C, H, L, MktCap, Va, Vo FROM bars WHERE Code IN {codes} AND AdjC>0 AND AdjO>0 ORDER BY Code, Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]; px['pos'] = tidx.reindex(px.Date).values
px['va20'] = px.groupby('Code').Va.transform(lambda s: s.shift(1).rolling(20, min_periods=10).median())
key = px.set_index(['Code', 'pos'])
def get(s, code, p): return s.reindex(list(zip(code, p))).values
code = ev.Code.values; rp = ev.rpos.values
for col in ['AdjO', 'AdjC', 'MktCap', 'va20', 'O', 'C', 'H', 'L', 'Vo']: ev[col] = get(key[col], code, rp)
ev['c_pre'] = get(key.AdjC, code, rp - 1)
ev['gap'] = ev.AdjO / ev.c_pre - 1
ev['ret'] = ev.AdjC / ev.AdjO - 1     # 寄り→引け（買い）
ev['mkt'] = tp.close.values[rp] / tp.open.values[rp] - 1
u = ev[(ev.MktCap >= 10000) & (ev.va20 >= 1e8) & ev.gap.notna() & ev.ret.notna()].copy()
u['period'] = np.where(u.rdate <= IS_END, 'IS', 'OOS')
# ストップ高・安で寄った（O==H==L==C）は建てられないので除く
u = u[~((u.O == u.H) & (u.H == u.L) & (u.L == u.C))]
print('events', len(u), u.groupby('period').size().to_dict(), u.rdate.min().date(), u.rdate.max().date())
u.to_parquet(f'{OUT}/p1_events.parquet')
COST = 0.00057 + 0.001
rows = []
print("\n== 横断: 反応日の寄り→引け（買い、bp、費用込み 15.7bp）。括弧は t。絶対 / TOPIX 中立")
for lo, hi, side in [(0.03, 9, '買い'), (0.05, 9, '買い'), (0.07, 9, '買い'), (0.10, 9, '買い'), (-9, -0.03, '買い'), (-9, -0.05, '買い'), (-9, -0.07, '買い'), (-9, -0.10, '買い'),
                     (0.03, 9, '売り'), (0.05, 9, '売り'), (-9, -0.03, '売り'), (-9, -0.05, '売り')]:
    m = (u.gap >= lo) & (u.gap < hi)
    sgn = 1 if side == '買い' else -1
    line = f"  ギャップ [{lo:+.2f},{hi:+.2f}) {side}:"
    for per in ['IS', 'OOS']:
        x = u[m & (u.period == per)]
        r = sgn * x.ret - COST; rn = sgn * (x.ret - x.mkt) - COST
        t = r.mean() / r.std() * np.sqrt(len(r)) if len(r) > 1 else np.nan
        tn = rn.mean() / rn.std() * np.sqrt(len(rn)) if len(rn) > 1 else np.nan
        line += f" {per} n={len(x):5d} {r.mean()*1e4:+6.0f}bp (t{t:+.1f}) / {rn.mean()*1e4:+6.0f}bp (t{tn:+.1f}) 勝率 {(r>0).mean():.2f} |"
        rows.append(dict(kind='xsec', lo=lo, hi=hi, side=side, period=per, n=len(x), bp=r.mean()*1e4, t=t, neu_bp=rn.mean()*1e4, neu_t=tn, win=(r>0).mean()))
    print(line)
# 十分位（ギャップ順、日ごとではなく全体）
print("\n== ギャップ十分位（全期間で区切り）の寄り→引け 買い bp（IS / OOS）")
u['dec'] = pd.qcut(u.gap, 10, labels=False) + 1
for d in range(1, 11):
    x = u[u.dec == d]
    s = []
    for per in ['IS', 'OOS']:
        y = x[x.period == per].ret - COST
        s.append(f"{per} {y.mean()*1e4:+5.0f}bp (t{y.mean()/y.std()*np.sqrt(len(y)):+.1f})")
    print(f"  D{d:2d} gap {x.gap.min()*100:+5.1f}〜{x.gap.max()*100:+5.1f}%  " + '  '.join(s))

# 籠: 各日 N=3、1 注文 100 万円、ギャップの大きい順（買い側）。滑り 10 / 20 bp
def basket(sel, sgn, n=3, budget=1e6, slip=0.001, asc=False):
    d = u[sel].copy(); d['sc'] = d.gap if not asc else -d.gap
    d = d.sort_values(['rdate', 'sc'], ascending=[True, False]).groupby('rdate').head(n)
    # 100 株単位
    d['shares'] = np.floor(budget / (d.O * 100)) * 100
    d = d[d.shares > 0]
    d['pnl'] = sgn * (d.C - d.O) * d.shares - (0.00057 + slip) * d.O * d.shares
    return d.groupby('rdate').pnl.sum().reindex(tdays[tdays >= '2017-01-01']).fillna(0), d
def pstats(p):
    eq = p.cumsum(); dd = (eq - eq.cummax()).min()
    return p.sum(), p.mean() / p.std() * np.sqrt(245) if p.std() > 0 else np.nan, dd
dt = pd.read_csv(f'{OUT}/trades_10y.csv'); dt['date'] = pd.to_datetime(dt.date)
dtd = dt.groupby('date').pnl.sum(); dtl = dt[dt.side == 'long'].groupby('date').pnl.sum()
print("\n== 籠: N=3・1 注文 100 万円")
for name, sel, sgn, asc in [('ギャップ ≥ +5% 買い', u.gap >= 0.05, 1, False), ('ギャップ ≥ +3% 買い', u.gap >= 0.03, 1, False),
                            ('ギャップ ≤ −5% 買い', u.gap <= -0.05, 1, True), ('ギャップ ≤ −3% 買い', u.gap <= -0.03, 1, True),
                            ('ギャップ ≤ −5% 売り', u.gap <= -0.05, -1, True)]:
    for slip in [0.001, 0.002]:
        p, d = basket(sel, sgn, slip=slip, asc=asc)
        a, b = split(p)
        sa, sb, s = pstats(a), pstats(b), pstats(p)
        corr = p.reindex(dtd.index).fillna(0).corr(dtd)
        yr = p.groupby(p.index.year).sum()
        rows.append(dict(kind='basket', name=name, slip=slip, pnl=s[0], sharpe=s[1], dd=s[2], is_pnl=sa[0], is_sharpe=sa[1], is_dd=sa[2], oos_pnl=sb[0], oos_sharpe=sb[1], oos_dd=sb[2], trades=len(d), corr_dt=corr, lose_years=int((yr < 0).sum())))
        print(f"  {name} 滑り{slip*1e4:.0f}bp: 取引 {len(d):5d}  合計 {s[0]:>12,.0f} 円  Sharpe {s[1]:5.2f}  DD {s[2]:>11,.0f} | IS {sa[0]:>11,.0f} / {sa[1]:.2f} / {sa[2]:,.0f} | OOS {sb[0]:>11,.0f} / {sb[1]:.2f} / {sb[2]:,.0f} | 負け年 {(yr<0).sum()} | daytrade 相関 {corr:+.2f}")
        if slip == 0.001: print("     年別:", {k: int(v/1e4) for k, v in yr.items()}, "万円")
pd.DataFrame(rows).to_csv(f'{OUT}/p1_earnings_day.csv', index=False)
