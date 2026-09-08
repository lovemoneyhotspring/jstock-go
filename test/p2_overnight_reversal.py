# 案 2. 短期リバーサルの持ち越し。当日 / 5 日リターンの最下位十分位を引けで買い、翌寄り・翌引け・3 日後の引けで売る。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
c = con()
tp = topix(c); tdays = tp.index
ev = pd.read_parquet(f'{OUT}/p1_events.parquet')[['Code', 'rdate']]
c.register('ev', ev)
COST = 0.00057 + 0.001; RATE = 0.028 / 245
df = c.execute("""
WITH b AS (
  SELECT Code, Date, AdjO, AdjC, MktCap, Va,
    median(Va) OVER (PARTITION BY Code ORDER BY Date ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) va20,
    lag(AdjC, 1) OVER w c1, lag(AdjC, 5) OVER w c5,
    lead(AdjO, 1) OVER w o_n1, lead(AdjC, 1) OVER w c_n1, lead(AdjC, 3) OVER w c_n3, lead(Date, 1) OVER w d_n1
  FROM bars WHERE AdjC > 0 AND AdjO > 0 WINDOW w AS (PARTITION BY Code ORDER BY Date)
)
SELECT b.*, (e1.Code IS NOT NULL) AS earn_today, (e2.Code IS NOT NULL) AS earn_next
FROM b LEFT JOIN ev e1 ON e1.Code = b.Code AND e1.rdate = b.Date
       LEFT JOIN ev e2 ON e2.Code = b.Code AND e2.rdate = b.d_n1
WHERE b.Date >= '2017-01-01' AND b.MktCap >= 10000 AND b.va20 >= 3e8 AND b.c5 IS NOT NULL AND b.c_n3 IS NOT NULL
""").df()
df['Date'] = pd.to_datetime(df.Date); df = df[df.Date.isin(tdays) & ~df.earn_today & ~df.earn_next].copy()
df['r1'] = df.AdjC / df.c1 - 1; df['r5'] = df.AdjC / df.c5 - 1
df['x_open'] = df.o_n1 / df.AdjC - 1 - COST - RATE
df['x_c1'] = df.c_n1 / df.AdjC - 1 - COST - RATE
df['x_c3'] = df.c_n3 / df.AdjC - 1 - COST - 3 * RATE
tpx = pd.DataFrame(dict(m_open=tp.open.shift(-1) / tp.close - 1, m_c1=tp.close.shift(-1) / tp.close - 1, m_c3=tp.close.shift(-3) / tp.close - 1))
df = df.join(tpx, on='Date')
df['period'] = np.where(df.Date <= IS_END, 'IS', 'OOS')
print('rows', len(df), df.Date.min().date(), df.Date.max().date(), '銘柄/日 平均', round(df.groupby('Date').size().mean()))
rows = []
for sig in ['r1', 'r5']:
    df['dec'] = df.groupby('Date')[sig].transform(lambda s: pd.qcut(s.rank(method='first'), 10, labels=False) + 1 if len(s) >= 50 else np.nan)
    print(f"\n== 信号 {sig}: 十分位 → 引け買いの平均 bp（費用込み）、括弧 t。絶対 / TOPIX 中立。D1 = 最も下げた")
    for ex, mk in [('x_open', 'm_open'), ('x_c1', 'm_c1'), ('x_c3', 'm_c3')]:
        for per in ['IS', 'OOS']:
            x = df[df.period == per]
            line = f"  {ex:6s} {per:3s}"
            for d in [1, 2, 5, 9, 10]:
                y = x[x.dec == d]; r = y[ex]; rn = y[ex] - y[mk]
                line += f"  D{d} {r.mean()*1e4:+5.0f}(t{r.mean()/r.std()*np.sqrt(len(r)):+.1f})/{rn.mean()*1e4:+5.0f}(t{rn.mean()/rn.std()*np.sqrt(len(rn)):+.1f})"
                rows.append(dict(kind='xsec', sig=sig, exit=ex, period=per, dec=d, n=len(r), bp=r.mean()*1e4, t=r.mean()/r.std()*np.sqrt(len(r)), neu_bp=rn.mean()*1e4, neu_t=rn.mean()/rn.std()*np.sqrt(len(rn))))
            print(line)
    # 籠: D1 のうち最も下げた N=10 を等金額、日次リターン系列
    for ex, hold in [('x_open', 1), ('x_c1', 1), ('x_c3', 3)]:
        pick = df[df.dec == 1].sort_values(['Date', sig]).groupby('Date').head(10)
        if hold == 1:
            r = pick.groupby('Date')[ex].mean().reindex(tdays).fillna(0)
            r.index = r.index  # 損益の計上日は翌日だが、統計は同じ
        else:
            # 3 日保有: 各日 1/3 の資金を投じ、3 日分の平均リターンを 3 で割って日次に均す（近似）
            r = (pick.groupby('Date')[ex].mean() / 3).reindex(tdays).fillna(0)
        tpr = tp.close.pct_change().reindex(tdays)
        a, b = split(r); ta, tb = split(tpr)
        sa, sb = stats(a), stats(b)
        rows.append(dict(kind='basket', sig=sig, exit=ex, period='IS', **sa)); rows.append(dict(kind='basket', sig=sig, exit=ex, period='OOS', **sb))
        print(f"  籠 D1 N=10 {ex:6s}: IS {fmt(sa)} | OOS {fmt(sb)}   TOPIX IS {fmt(stats(ta))} | OOS {fmt(stats(tb))}")
pd.DataFrame(rows).to_csv(f'{OUT}/p2_overnight_reversal.csv', index=False)
