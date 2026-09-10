import sys; sys.path.insert(0,'.')
from common import *
t = pd.read_csv(f'{OUT}/trades_10y.csv'); t['date']=pd.to_datetime(t.date)
t['bp'] = t.pnl/t.amount*1e4
print(f"全体 {len(t)} 取引  損益 {t.pnl.sum():,.0f} 円  平均 {t.bp.mean():.1f} bp")
for side, g in t.groupby('side'):
    print(f"\n--- {side}: {len(g)} 取引  損益 {g.pnl.sum():,.0f} 円  平均 {g.bp.mean():+.1f} bp  勝率 {(g.pnl>0).mean()*100:.0f}%")
    q = pd.qcut(g.gap.abs(), 5, labels=[f'Q{i}' for i in range(1,6)])
    a = g.groupby(q, observed=True).agg(n=('pnl','size'), gap=('gap','mean'), bp=('bp','mean'), pnl=('pnl','sum'))
    a['t'] = g.groupby(q, observed=True).bp.apply(lambda x: x.mean()/x.std()*np.sqrt(len(x)))
    print("  ギャップの大きさ 5 分位（|gap| 小→大）")
    for k,r in a.iterrows(): print(f"   {k} gap {r.gap*100:+5.2f}%  {int(r.n):4d} 件  {r.bp:+6.1f} bp (t {r.t:4.1f})  損益 {r.pnl:>12,.0f}")
    b = g.groupby('rank').agg(n=('pnl','size'), bp=('bp','mean'), pnl=('pnl','sum'))
    print("  順位:", "  ".join(f"{int(k)}位 {r.bp:+.1f}bp/{r.pnl:,.0f}円" for k,r in b.iterrows()))
# 市場ギャップ（TOPIX 寄り ÷ 前日終値 − 1）
c = con(); ix = topix(c); ix['gap'] = ix.open/ix.close.shift(1)-1
d = t.groupby(['date','side']).pnl.sum().unstack().fillna(0); d['all']=d.sum(axis=1)
d = d.join(ix.gap.rename('mgap'))
bins = [-1,-0.02,-0.01,-0.003,0.003,1]; lab=['≤-2%','-2〜-1%','-1〜-0.3%','-0.3〜+0.3%','≥+0.3%']
q = pd.cut(d.mgap, bins, labels=lab)
print("\n--- 市場ギャップ（TOPIX 寄り÷前日終値）別の 1 日あたり損益")
for k,g in d.groupby(q, observed=True):
    print(f"  {k:<12} {len(g):4d} 日  合算 {g['all'].mean():>9,.0f} 円/日 (計 {g['all'].sum():>12,.0f})  ロング {g.get('long',pd.Series(0)).mean():>9,.0f}  ショート {g.get('short',pd.Series(0)).mean():>8,.0f}")
print("\n--- 月別の 1 日あたり合算損益")
m = d.groupby(d.index.month)['all'].agg(['size','sum','mean'])
print("  " + "  ".join(f"{int(k)}月 {r['mean']:>8,.0f}" for k,r in m.iterrows()))
