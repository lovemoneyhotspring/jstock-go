# 案 4. ロング脚のベータヘッジ。日次のロング建玉 × h の TOPIX を寄付で売り引けで買い戻す。ショック日（scale≠1）は外す変種も。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
c = con(); tp = topix(c)
t = pd.read_csv(f'{OUT}/trades_10y.csv'); t['date'] = pd.to_datetime(t.date)
lg = t[t.side == 'long']; sh = t[t.side == 'short']
d = pd.DataFrame(dict(long_pnl=lg.groupby('date').pnl.sum(), long_amt=lg.groupby('date').amount.sum(), scale=lg.groupby('date').scale.max(),
                      short_pnl=sh.groupby('date').pnl.sum())).fillna(0)
d['mkt'] = (tp.close / tp.open - 1).reindex(d.index)
d = d.dropna(subset=['mkt'])
d['period'] = np.where(d.index <= IS_END, 'IS', 'OOS')
d['long_ret'] = d.long_pnl / d.long_amt.replace(0, np.nan)
print('days', len(d), d.index.min().date(), d.index.max().date(), 'ショック日（scale≠1）', (d.scale != 1).sum())
for per in ['IS', 'OOS', 'ALL']:
    x = d if per == 'ALL' else d[d.period == per]
    x = x[x.long_amt > 0]
    beta = np.polyfit(x.mkt, x.long_ret, 1)[0]
    print(f"  {per}: ロングの日次リターン vs TOPIX 寄→引 相関 {x.mkt.corr(x.long_ret):+.2f} β {beta:.2f}  TOPIX 日中平均 {x.mkt.mean()*1e4:+.1f}bp")
def pstats(p):
    eq = p.cumsum(); dd = (eq - eq.cummax()); mdd = dd.min()
    # 最長 DD（日数）
    under = dd < 0; runs = under.astype(int).groupby((~under).cumsum()).sum(); longest = runs.max()
    yr = p.groupby(p.index.year).sum()
    return dict(pnl=p.sum(), sharpe=p.mean() / p.std() * np.sqrt(245), dd=mdd, longest=longest, lose=int((yr < 0).sum()))
rows = []
print("\n== 合算（ロング + ショート + ヘッジ）。ヘッジ費用 往復 1bp")
base = d.long_pnl + d.short_pnl
for skip_shock in [False, True]:
    for h in [0, 0.25, 0.5, 0.75, 1.0]:
        hh = np.where(skip_shock & (d.scale != 1), 0, h)
        hedge = -hh * d.long_amt * d.mkt - hh * d.long_amt * 0.0001
        p = base + hedge
        a, b = split(p)
        sa, sb, s = pstats(a), pstats(b), pstats(p)
        rows.append(dict(skip_shock=skip_shock, h=h, **{k: v for k, v in s.items()}, **{'is_' + k: v for k, v in sa.items()}, **{'oos_' + k: v for k, v in sb.items()}, hedge_pnl=hedge.sum()))
        print(f"  {'ショック日除外' if skip_shock else '常時      '} h={h:4.2f}: 損益 {s['pnl']:>12,.0f} (ヘッジ {hedge.sum():>+11,.0f}) Sharpe {s['sharpe']:.2f} DD {s['dd']:>11,.0f} 最長 {s['longest']:4d}日 負け年 {s['lose']} | IS {sa['pnl']:>11,.0f} / {sa['sharpe']:.2f} / {sa['dd']:>10,.0f} | OOS {sb['pnl']:>11,.0f} / {sb['sharpe']:.2f} / {sb['dd']:>10,.0f}")
# ロング脚だけ
print("\n== ロング脚だけ + ヘッジ")
for h in [0, 0.5, 1.0]:
    hedge = -h * d.long_amt * d.mkt - h * d.long_amt * 0.0001
    p = d.long_pnl + hedge; a, b = split(p); sa, sb, s = pstats(a), pstats(b), pstats(p)
    print(f"  h={h:4.2f}: 損益 {s['pnl']:>12,.0f} Sharpe {s['sharpe']:.2f} DD {s['dd']:>11,.0f} 最長 {s['longest']:4d}日 負け年 {s['lose']} | IS {sa['pnl']:>11,.0f} / {sa['sharpe']:.2f} | OOS {sb['pnl']:>11,.0f} / {sb['sharpe']:.2f}")
    print("     年別(万円):", {k: int(v / 1e4) for k, v in p.groupby(p.index.year).sum().items()})
# 先物ミニ 1 枚（TOPIX × 1,000 円）に丸めた場合
print("\n== TOPIX 先物ミニ 1 枚単位に丸め（h=0.5・1.0、常時）")
lvl = tp.open.reindex(d.index)
for h in [0.5, 1.0]:
    n = np.round(h * d.long_amt / (lvl * 1000))
    hedge = -n * 1000 * (tp.close.reindex(d.index) - lvl) - n * lvl * 1000 * 0.0001
    p = base + hedge; s = pstats(p); a, b = split(p); sa, sb = pstats(a), pstats(b)
    print(f"  h={h}: 枚数 平均 {n.mean():.1f}  損益 {s['pnl']:>12,.0f} Sharpe {s['sharpe']:.2f} DD {s['dd']:>11,.0f} | IS {sa['sharpe']:.2f} OOS {sb['sharpe']:.2f}")
pd.DataFrame(rows).to_csv(f'{OUT}/p4_beta_hedge.csv', index=False)
