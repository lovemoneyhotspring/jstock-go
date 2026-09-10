# 225 オプションの売り。SQ 翌営業日に翌限月の OTM プット（+コール）を売り、SQ まで持つ。日次は清算値で評価。
import sys; sys.path.insert(0,'.')
from common import *
import itertools
c=con()
o=c.execute(f"""SELECT Date, CM, LTD, SQD, PCDiv, TRY_CAST(Strike AS DOUBLE) K, TRY_CAST(Settle AS DOUBLE) S, TRY_CAST(UnderPx AS DOUBLE) U, TRY_CAST(Vo AS DOUBLE) Vo
FROM read_parquet('{ROOT}/derivatives_bars_daily_options_225/*.parquet', union_by_name=true)""").df()
for k in ['Date','LTD','SQD']: o[k]=pd.to_datetime(o[k])
o=o[o.S.notna()&(o.K>0)]
tdays=sorted(o.Date.unique()); tpos={d:i for i,d in enumerate(tdays)}
n225=wb('^N225')
sqs=sorted(o.SQD.unique()); sqs=[s for s in sqs if s>=pd.Timestamp('2016-09-01') and s<=pd.Timestamp('2026-08-31')]
CAP=3_000_000; MULT=1000; FEE=500; TICK=5
def sq_value(sqd):
    if sqd in n225.index: return n225.open[sqd]
    x=o[(o.Date==sqd)].U; return x.iloc[0] if len(x) else np.nan
def run(d, strangle):
    daily=pd.Series(0.0,index=pd.DatetimeIndex(tdays)); legs=[]
    for i in range(len(sqs)-1):
        sqd=sqs[i]; nxt=sqs[i+1]
        # 建て日 = SQ の翌営業日
        after=[t for t in tdays if t>sqd]
        if not after: break
        e=after[0]; chain=o[(o.Date==e)&(o.SQD==nxt)]
        if chain.empty: continue
        U=chain.U.iloc[0]
        puts=chain[(chain.PCDiv=='1')&(chain.K<=U*(1-d))].sort_values('K')
        calls=chain[(chain.PCDiv=='2')&(chain.K>=U*(1+d))].sort_values('K')
        sel=[]
        if len(puts): sel.append(('1',puts.K.iloc[-1]))
        if strangle and len(calls): sel.append(('2',calls.K.iloc[0]))
        sqv=sq_value(nxt)
        for pc,K in sel:
            path=o[(o.SQD==nxt)&(o.PCDiv==pc)&(o.K==K)&(o.Date>=e)&(o.Date<=nxt)].sort_values('Date')
            if path.empty: continue
            s=path.set_index('Date').S
            entry=s.iloc[0]-TICK
            # 日次損益: 建て日は entry - S(e)、以後は -(S_t - S_{t-1})、SQ 日は本質価値で決済
            intrinsic=max(0.0,(K-sqv) if pc=='1' else (sqv-K)) if not np.isnan(sqv) else s.iloc[-1]
            vals=list(s.values); dates=list(s.index)
            if dates[-1]<nxt or True:
                vals.append(intrinsic); dates.append(nxt if nxt>dates[-1] else dates[-1]+pd.Timedelta(days=1))
            pnl=[(entry-vals[0])*MULT-FEE]+[-(vals[j]-vals[j-1])*MULT for j in range(1,len(vals))]
            for dt,p in zip(dates,pnl): daily[dt]=daily.get(dt,0)+p
            legs.append(dict(entry=e,sq=nxt,pc=pc,K=K,U=U,prem=entry,sqv=sqv,pnl=sum(pnl)))
    return daily[daily.index>=pd.Timestamp('2016-09-01')], pd.DataFrame(legs)
# daytrade の日次損益（現行設定 10 年）
t=pd.read_csv('out/trades_10y.csv'); t['date']=pd.to_datetime(t.date); dt=t.groupby('date').pnl.sum()
def st(daily, cap):
    r=daily/cap; eq=daily.cumsum(); dd=(eq-eq.cummax()).min()
    yrs=len(daily)/245; return dict(annual=daily.sum()/yrs/cap*100, sharpe=r.mean()/r.std()*np.sqrt(245), dd=dd, total=daily.sum())
idx=pd.DatetimeIndex(sorted(set(tdays)|set(dt.index))); dtd=dt.reindex(idx).fillna(0)
base=st(dtd[dtd.index>='2016-09-01'],2_000_000); print(f"daytrade 単独: 年率 {base['annual']:.1f}%（対 200 万）  Sharpe {base['sharpe']:.2f}  最大DD {base['dd']:,.0f}  合計 {base['total']:,.0f}")
rows=[]
for d,strg in itertools.product([0.05,0.07,0.10],[True,False]):
    daily,legs=run(d,strg); daily=daily.reindex(idx).fillna(0)
    s=st(daily,CAP); corr=np.corrcoef(daily.values, dtd.values)[0,1]
    m=daily.resample('ME').sum(); dm=dtd.resample('ME').sum(); mcorr=np.corrcoef(m.values, dm.reindex(m.index).fillna(0).values)[0,1]
    comb=st(daily+dtd, 2_000_000+CAP)
    lab=f"d{int(d*100)} {'ストラングル' if strg else 'プットのみ'}"
    worst=legs.nsmallest(3,'pnl')[['sq','pnl']].values.tolist()
    print(f"{lab:14s} 年率 {s['annual']:5.1f}%（対300万） Sharpe {s['sharpe']:5.2f} DD {s['dd']:>11,.0f} 合計 {s['total']:>12,.0f} | 相関 日次 {corr:+.2f} 月次 {mcorr:+.2f} | 合算 DD {comb['dd']:>11,.0f} Sharpe {comb['sharpe']:.2f} | 最悪 {[(str(w[0])[:7],int(w[1])) for w in worst]}")
    rows.append(dict(label=lab,**s,corr=corr,mcorr=mcorr,comb_dd=comb['dd'],comb_sharpe=comb['sharpe'],legs=len(legs)))
g=pd.DataFrame(rows); g.to_csv('out/opt_premium.csv',index=False)
print(f"中央値: 年率 {g.annual.median():.1f}%  相関(日次) {g['corr'].median():+.2f}  合算DD {g.comb_dd.median():,.0f}  vs daytrade単独 DD {base['dd']:,.0f}")
# 参考: ショック日（TOPIX 寄りギャップ ≤ −2%）の ETF 日次リターン（代用有価証券の目減り）
tp=topix(c); gap=tp.open/tp.close.shift(1)-1; shock=gap[gap<=-0.02].index
print("\nショック日（市場ギャップ ≤ −2%）", len(shock), "日の ETF の当日リターン（前日終値→当日終値）と翌日")
for code,name in [('13060','TOPIX'),('13210','日経225'),('16550','S&P500円'),('15450','NASDAQ100円')]:
    e=jp_etf(c,code); r=e.close.pct_change(); rs=r.reindex(shock).dropna(); r1=r.shift(-1).reindex(shock).dropna()
    print(f"  {name:12s} n={len(rs)}  当日平均 {rs.mean()*100:+.2f}%  最悪 {rs.min()*100:+.2f}%  翌日平均 {r1.mean()*100:+.2f}%  10年 CAGR {stats(r.dropna())['cagr']:.1f}%  DD {stats(r.dropna())['dd']:.0f}%")
