# 市場水準の損切り: 判定時刻 T に 1306 が寄りから X% 以上下げていたら、その日のロングを T の成行で手仕舞う。
import duckdb, pandas as pd, numpy as np, os, itertools
ROOT=os.path.expanduser('~/jstock-go/data/jquants')
t=pd.read_csv('out/trades_2y.csv'); t=t[t.side=='long'].copy(); t['date']=pd.to_datetime(t.date); t['code']=t.code.astype(str)
con=duckdb.connect(); con.register('t', t[['date','code']].drop_duplicates())
codes="','".join(sorted(set(t.code)|{'13060'}))
import glob
files=sorted(glob.glob(f'{ROOT}/equities_bars_minute/*.parquet'))
parts=[]
for f in files:
    parts.append(con.execute(f"""
SELECT Date, Code, Time, TRY_CAST(O AS DOUBLE) O, TRY_CAST(C AS DOUBLE) C
FROM read_parquet('{f}') WHERE Code IN ('{codes}') AND Date >= '2024-09-02' AND TRY_CAST(Vo AS DOUBLE) > 0
  AND (Code = '13060' OR Time BETWEEN '09:00' AND '09:05' OR Time BETWEEN '09:59' AND '10:05' OR Time BETWEEN '11:29' AND '11:31' OR Time BETWEEN '12:59' AND '13:05' OR Time BETWEEN '13:59' AND '14:05' OR Time BETWEEN '14:59' AND '15:05' OR Time BETWEEN '15:19' AND '15:30')
""").df())
m=pd.concat(parts, ignore_index=True); del parts; m['Date']=pd.to_datetime(m.Date)
print('minute rows', len(m), flush=True)
def first_at(df, tm):  # 各 (Date, Code) で tm 以降の最初の約定の始値
    x=df[df.Time>=tm].sort_values('Time').groupby(['Date','Code']).first()
    return x.O
mk=m[m.Code!='13060']; ix=m[m.Code=='13060'].sort_values('Time')
idx_open=ix.groupby('Date').first().O
def idx_at(tm): return ix[ix.Time<=tm].groupby('Date').last().C
entry=first_at(mk,'09:01'); exit_=first_at(mk,'15:20')
t=t.set_index(['date','code'])
t['e']=entry.reindex(t.index).values; t['x']=exit_.reindex(t.index).values
t=t.dropna(subset=['e','x'])
t['pnl0']=(t.x-t.e)*t.shares
def daily_stats(p):
    d=p.groupby(level=0).sum(); eq=d.cumsum(); dd=(eq-eq.cummax()).min()
    return d.sum(), d.mean()/d.std()*np.sqrt(245), dd
b=daily_stats(t.pnl0); print(f"損切りなし: 損益 {b[0]:,.0f} 円  Sharpe {b[1]:.2f}  最大DD {b[2]:,.0f} 円  取引 {len(t)}")
rows=[]
for X,T in itertools.product([-0.01,-0.015,-0.02],['13:00','14:00','15:00']):
    mkt=(idx_at(T)/idx_open-1)
    trig=mkt[mkt<=X].index
    stop=first_at(mk,T)
    p=t.pnl0.copy(); hit=t.index.get_level_values(0).isin(trig)
    sp=stop.reindex(t.index).values
    p[hit]=((sp-t.e)*t.shares)[hit]
    s=daily_stats(p); n=len(trig); nt=hit.sum()
    rows.append(dict(X=X,T=T,days=n,trades=nt,pnl=s[0],sharpe=s[1],dd=s[2],d_pnl=s[0]-b[0],d_dd=s[2]-b[2],
                     hit_pnl0=t.pnl0[hit].sum(), hit_pnl=p[hit].sum()))
    print(f"X {X*100:+.1f}% T {T}: 発動 {n:3d} 日 / {nt:3d} 取引  損益 {s[0]:,.0f} ({s[0]-b[0]:+,.0f})  Sharpe {s[1]:.2f}  DD {s[2]:,.0f} ({s[2]-b[2]:+,.0f})  発動日の損益 なし {t.pnl0[hit].sum():,.0f} → あり {p[hit].sum():,.0f}")
g=pd.DataFrame(rows); g.to_csv('out/dt_market_stop_late.csv',index=False)
v=g[g.trades>=20]
print(f"\n格子 {len(g)}（発動 20 回以上 {len(v)}）: 損益の変化 中央値 {v.d_pnl.median():+,.0f} 円、改善した数 {(v.d_pnl>0).sum()}/{len(v)}、DD の変化 中央値 {v.d_dd.median():+,.0f} 円、改善 {(v.d_dd>0).sum()}/{len(v)}")
# 参考: 発動日の市場の寄り→引けと、ロングの寄り→T、T→引けの分解
mkt_close=idx_at('15:20')/idx_open-1
for X,T in [(-0.01,'14:00'),(-0.01,'15:00'),(-0.015,'14:00'),(-0.015,'15:00')]:
    mkt=(idx_at(T)/idx_open-1); trig=mkt[mkt<=X].index
    hit=t.index.get_level_values(0).isin(trig); sp=stop.reindex(t.index).values if False else first_at(mk,T).reindex(t.index).values
    a=((sp-t.e)*t.shares)[hit].sum(); b2=((t.x-sp)*t.shares)[hit].sum()
    print(f"  X {X*100:+.1f}% T {T}: 発動日 {len(trig)} 日  市場の T→15:20 平均 {mkt_close.reindex(trig).mean()*100-mkt.reindex(trig).mean()*100:+.2f}%  ロング 寄→T {a:,.0f} 円  T→15:20 {b2:,.0f} 円")
