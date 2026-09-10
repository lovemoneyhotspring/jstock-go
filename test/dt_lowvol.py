import sys; sys.path.insert(0,'.')
from common import *
t=pd.read_csv(f'{OUT}/trades_10y.csv'); t['date']=pd.to_datetime(t.date); t['bp']=t.pnl/t.amount*1e4
c=con(); ix=topix(c); vol=(ix.close.pct_change().rolling(60).std()*np.sqrt(245)).shift(1)
t=t.join(vol.rename('mvol'), on='date'); hv=vol.reindex(t.date.unique()).median()
lo=t[t.mvol<=hv]; hi=t[t.mvol>hv]
print(f"60 日ボラの中央値 {hv*100:.1f}%")
for lab,g in [('低ボラ',lo),('高ボラ',hi)]:
    L=g[g.side=='long']
    q=pd.qcut(L.gap,5)
    print(f"\n--- {lab}: ロング {len(L)} 取引  {L.pnl.sum():,.0f} 円  平均 {L.bp.mean():+.1f} bp")
    for k,x in L.groupby(q,observed=True):
        print(f"   gap {k.left*100:+6.2f}〜{k.right*100:+6.2f}%  {len(x):4d} 件  {x.bp.mean():+6.1f} bp (t {x.bp.mean()/x.bp.std()*np.sqrt(len(x)):4.1f})  計 {x.pnl.sum():>11,.0f}")
    S=g[g.side=='short']
    print(f"   ショート {len(S)} 件 {S.pnl.sum():,.0f} 円 平均 {S.bp.mean():+.1f} bp")
print("\n=== 「その日の最も深いギャップ」で日を分ける（ロングのみ、低ボラ日）")
L=t[t.side=='long']
day=L.groupby('date').agg(deep=('gap','min'), pnl=('pnl','sum'), n=('pnl','size')).join(vol.rename('mvol'))
for lab,d in [('低ボラ日', day[day.mvol<=hv]), ('高ボラ日', day[day.mvol>hv])]:
    print(f"--- {lab} {len(d)} 日")
    q=pd.qcut(d.deep,5)
    for k,x in d.groupby(q,observed=True):
        print(f"   最深ギャップ {k.left*100:+6.2f}〜{k.right*100:+6.2f}%  {len(x):3d} 日  {x.pnl.mean():>8,.0f} 円/日  計 {x.pnl.sum():>11,.0f}")
print("\n=== 閾値ルール: 最深ギャップが -X% より浅い日はロングを見送る（全期間・ロングのみ）")
base=day.pnl.sum()
for X in [0.02,0.025,0.03,0.035,0.04]:
    keep=day[day.deep<=-X]
    print(f"  X={X*100:.1f}%: 取引日 {len(keep):4d}/{len(day)}  損益 {keep.pnl.sum():>11,.0f}（{keep.pnl.sum()-base:+,.0f}）  円/日 {keep.pnl.mean():>8,.0f}  見送った日の損益 {day[day.deep>-X].pnl.sum():>10,.0f}")
