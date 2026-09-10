import sys, glob, os; sys.path.insert(0,'.')
from common import *
WB=os.path.expanduser('~/webull/wbjp/data/bars')
syms=[os.path.basename(f)[:-8] for f in glob.glob(f'{WB}/*.parquet')]
us=[s for s in syms if s.isalpha() and s.isupper() and not s.startswith('^')]
print(f"米国株 {len(us)} 銘柄（生存母集団）")
fr=[]
for s in us:
    d=pd.read_parquet(f'{WB}/{s}.parquet')[['date','open','high','low','close','volume']]
    d['date']=pd.to_datetime(d.date).dt.tz_localize(None); d['sym']=s; fr.append(d)
b=pd.concat(fr).sort_values(['sym','date'])
b['pc']=b.groupby('sym').close.shift(1)
b['gap']=b.open/b.pc-1; b['oc']=b.close/b.open-1
b['dv']=(b.close*b.volume).groupby(b.sym).transform(lambda x: x.rolling(20).median())
b=b.dropna(subset=['gap','oc','dv'])
b=b[(b.dv>2e7)&(b.pc>5)]        # 20 日売買代金 2,000 万ドル以上
COST=0.0010                      # 往復 10 bp（米国株は日本より安いが保守的に）
# 各日、ギャップ下位 3 銘柄を寄りで買い引けで売る
b=b[b.gap<0]
b['rk']=b.groupby('date').gap.rank()
pick=b[b.rk<=3].copy(); pick['ret']=pick.oc-COST
daily=pick.groupby('date').ret.mean()
print(f"米国ギャップ逆張り（下位 3、往復 10 bp）: {len(daily)} 日  平均 {daily.mean()*1e4:+.1f} bp/日  t {daily.mean()/daily.std()*np.sqrt(len(daily)):.1f}  年率 {daily.mean()*252*100:+.1f}%")
sp=pd.read_parquet(f'{WB}/SPY.parquet'); sp['date']=pd.to_datetime(sp.date).dt.tz_localize(None); sp=sp.set_index('date')
spv=(sp.close.pct_change().rolling(60).std()*np.sqrt(252)).shift(1)
# 日本の局面
c=con(); ix=topix(c); lp=np.log(ix.close); W=60
x=np.arange(W); xd=x-x.mean(); sxx=(xd**2).sum(); v=lp.values; sl=[];r2=[]
for i in range(len(v)):
    if i<W-1: sl.append(np.nan); r2.append(np.nan); continue
    y=v[i-W+1:i+1]; bb=(xd*(y-y.mean())).sum()/sxx; res=y-(y.mean()-bb*x.mean()+bb*x)
    sl.append(bb); r2.append(1-res.var()/y.var())
reg=pd.DataFrame({'slope':sl,'r2':r2},index=ix.index)
reg['jvol']=ix.close.pct_change().rolling(W).std()*np.sqrt(245); reg=reg.shift(1)
hv=reg.jvol.median()
def cls(r):
    if r.r2>=0.5: return '明確な上昇' if r.slope>0 else '明確な下降'
    return '方向なし'
reg['c3']=reg.apply(cls,axis=1); reg['hv']=np.where(reg.jvol>hv,'高ボラ','低ボラ')
j=pd.DataFrame({'us':daily}).join(spv.rename('svol')).join(reg[['c3','hv','jvol']])
j=j.dropna(subset=['c3'])
print(f"\n=== 日本のボラと米国のボラの重なり（60 日実現ボラ、{j.index.min().date()}〜{j.index.max().date()}）")
print(f"  相関 {j.jvol.corr(j.svol):.2f}")
print(f"  日本が低ボラの日の米国ボラ 中央値 {j[j.hv=='低ボラ'].svol.median()*100:.1f}%  ／ 日本が高ボラの日 {j[j.hv=='高ボラ'].svol.median()*100:.1f}%")
lo_both=((j.hv=='低ボラ')&(j.svol<=j.svol.median())).mean(); print(f"  日本が低ボラの日のうち米国も低ボラ: {((j.svol<=j.svol.median())[j.hv=='低ボラ']).mean()*100:.0f}%")
print("\n=== 米国ギャップ逆張りを日本の局面で分けると")
for k,g in j.groupby(['c3','hv'],observed=True):
    print(f"  {k[0]} × {k[1]:<4} {len(g):4d} 日  {g.us.mean()*1e4:+6.1f} bp/日  t {g.us.mean()/g.us.std()*np.sqrt(len(g)):4.1f}  年率 {g.us.mean()*252*100:+6.1f}%")
print("\n=== 米国ギャップ逆張りを米国のボラで分けると")
q=pd.qcut(j.svol,4)
for k,g in j.groupby(q,observed=True):
    print(f"  米ボラ {k.left*100:4.1f}〜{k.right*100:4.1f}%  {len(g):4d} 日  {g.us.mean()*1e4:+6.1f} bp/日  t {g.us.mean()/g.us.std()*np.sqrt(len(g)):4.1f}")

print("\n=== 米国ギャップ逆張りの年代別（往復 10 bp 込み）")
dd=pd.DataFrame({'r':daily})
for a,b_ in [('1996','2004'),('2005','2009'),('2010','2015'),('2016','2020'),('2021','2026')]:
    g=dd[(dd.index>=a)&(dd.index<=b_+'-12-31')]
    if len(g): print(f"  {a}〜{b_}  {len(g):4d} 日  {g.r.mean()*1e4:+6.1f} bp/日  t {g.r.mean()/g.r.std()*np.sqrt(len(g)):5.1f}  年率 {g.r.mean()*252*100:+6.1f}%")
print("\n=== 参考: 同じ期間に日本の daytrade（現行規模）")
t=pd.read_csv(f'{OUT}/trades_10y.csv'); t['date']=pd.to_datetime(t.date)
jd=t.groupby('date').pnl.sum()
print(f"  2017-01〜2026-09  {len(jd)} 取引日  {jd.mean():,.0f} 円/日（建玉 500 万に対し {jd.mean()/5e6*1e4:+.1f} bp/日）")
