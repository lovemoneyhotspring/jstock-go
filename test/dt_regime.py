import sys; sys.path.insert(0,'.')
from common import *
t = pd.read_csv(f'{OUT}/trades_10y.csv'); t['date']=pd.to_datetime(t.date)
d = t.groupby(['date','side']).pnl.sum().unstack().fillna(0); d['all']=d.sum(axis=1)
c=con(); ix=topix(c); lp=np.log(ix.close)
W=60
x=np.arange(W); xd=x-x.mean(); sxx=(xd**2).sum()
sl=[]; r2=[]; 
v=lp.values
for i in range(len(v)):
    if i<W-1: sl.append(np.nan); r2.append(np.nan); continue
    y=v[i-W+1:i+1]; b=(xd*(y-y.mean())).sum()/sxx; a=y.mean()-b*x.mean()
    res=y-(a+b*x); ss=((y-y.mean())**2).sum()
    sl.append(b*245); r2.append(1-res.var()/y.var() if y.var()>0 else np.nan)
reg=pd.DataFrame({'slope':sl,'r2':r2}, index=ix.index)
reg['vol']=ix.close.pct_change().rolling(W).std()*np.sqrt(245)
reg['ret60']=ix.close/ix.close.shift(W)-1
reg=reg.shift(1)                       # 前日までの情報で当日を分類
j=d.join(reg, how='inner').dropna(subset=['slope'])
allbiz=ix.join(reg).dropna(subset=['slope']); allbiz=allbiz[(allbiz.index>=d.index[0])&(allbiz.index<=d.index[-1])]

def show(g, lab):
    print(f"  {lab:<26} {len(g):4d} 日  {g['all'].mean():>9,.0f} 円/日  計 {g['all'].sum():>11,.0f}  ロング {g['long'].mean():>8,.0f}  ショート {g['short'].mean():>8,.0f}  勝ち日 {(g['all']>0).mean()*100:.0f}%")

print("=== 60 日の傾き 5 分位（TOPIX、前日まで）")
q=pd.qcut(j.ret60,5)
for k,g in j.groupby(q,observed=True): show(g, f"{k.left*100:+.0f}〜{k.right*100:+.0f}%")
print("\n=== トレンドの明確さ（60 日回帰の R²）× 向き")
def cls(r):
    if r.r2>=0.5: return '明確な上昇' if r.slope>0 else '明確な下降'
    if r.r2>=0.2: return 'ゆるい上昇' if r.slope>0 else 'ゆるい下降'
    return '方向なし（乱高下・レンジ）'
j['reg']=j.apply(cls,axis=1)
for k in ['明確な上昇','ゆるい上昇','方向なし（乱高下・レンジ）','ゆるい下降','明確な下降']:
    g=j[j.reg==k]
    if len(g): show(g,k)
print("\n=== ボラ（60 日実現ボラ）× 向きの 2×2")
hv=j.vol.median()
for lab,g in [('高ボラ×上昇', j[(j.vol>hv)&(j.slope>0)]), ('高ボラ×下降', j[(j.vol>hv)&(j.slope<=0)]),
              ('低ボラ×上昇', j[(j.vol<=hv)&(j.slope>0)]), ('低ボラ×下降', j[(j.vol<=hv)&(j.slope<=0)])]:
    show(g,lab)
print(f"  （60 日ボラの中央値 {hv*100:.1f}%）")
print("\n=== 稼働率（危険信号で休む日を含む。分母は全営業日）")
for k in ['明確な上昇','ゆるい上昇','方向なし（乱高下・レンジ）','ゆるい下降','明確な下降']:
    ab=allbiz[allbiz.apply(cls,axis=1)==k]
    if len(ab): print(f"  {k:<26} 営業日 {len(ab):4d}  取引日 {j[j.reg==k].shape[0]:4d}  稼働 {j[j.reg==k].shape[0]/len(ab)*100:3.0f}%")
print("\n=== 代表的な期間")
PER=[('2017-01-06','2018-01-23','上昇（2017 の全面高）'),('2018-01-24','2018-12-25','下降（VIX ショック〜年末）'),
     ('2019-01-01','2020-01-31','上昇（貿易戦争明け）'),('2020-02-01','2020-04-30','暴落（コロナ）'),
     ('2020-05-01','2021-09-30','上昇（コロナ後の全面高）'),('2021-10-01','2022-12-31','レンジ・下降（利上げ）'),
     ('2023-01-01','2024-07-11','上昇（日本株の再評価）'),('2024-07-12','2024-09-30','急落（8/5 ブラックマンデー）'),
     ('2024-10-01','2025-09-30','上昇'),('2025-10-01','2026-02-28','上昇（戦略の最大 DD 期）'),
     ('2026-03-01','2026-09-07','直近')]
for a,b,lab in PER:
    a,b=pd.Timestamp(a),pd.Timestamp(b); g=j[(j.index>=a)&(j.index<=b)]; m=ix[(ix.index>=a)&(ix.index<=b)]
    if not len(g): continue
    tr=m.close.iloc[-1]/m.close.iloc[0]-1
    print(f"  {a.date()}〜{b.date()} {lab:<26} TOPIX {tr*100:+6.1f}%  戦略 {g['all'].sum():>11,.0f} 円（{g['all'].mean():>8,.0f}/日、{len(g)} 日、ロング {g['long'].sum():>10,.0f} / ショート {g['short'].sum():>9,.0f}）")

print("\n=== トレンドの向き × ボラ（60 日）の 3×2")
def cls3(r):
    if r.r2>=0.5: return '明確な上昇' if r.slope>0 else '明確な下降'
    return '方向なし'
j['c3']=j.apply(cls3,axis=1); j['hv']=np.where(j.vol>hv,'高ボラ','低ボラ')
for k,g in j.groupby(['c3','hv'],observed=True):
    show(g, f"{k[0]} × {k[1]}")
print("\n=== 代表期間の 60 日ボラ（平均）と稼働")
for a,b,lab in PER:
    a,b=pd.Timestamp(a),pd.Timestamp(b); g=j[(j.index>=a)&(j.index<=b)]
    if not len(g): continue
    ab=allbiz[(allbiz.index>=a)&(allbiz.index<=b)]
    print(f"  {lab:<26} ボラ {g.vol.mean()*100:5.1f}%  稼働 {len(g)/len(ab)*100:3.0f}%  {g['all'].mean():>8,.0f} 円/日")

print("\n=== 3×2 を「全営業日あたり」で（休んだ日も分母に入れる）")
ab=allbiz.copy(); ab['c3']=ab.apply(cls3,axis=1); ab['hv']=np.where(ab.vol>hv,'高ボラ','低ボラ')
for k,g in j.groupby(['c3','hv'],observed=True):
    n=len(ab[(ab.c3==k[0])&(ab.hv==k[1])])
    print(f"  {k[0]} × {k[1]:<6} 営業日 {n:4d}  取引日 {len(g):4d}（稼働 {len(g)/n*100:3.0f}%）  {g['all'].sum()/n:>8,.0f} 円/営業日  計 {g['all'].sum():>11,.0f}")
