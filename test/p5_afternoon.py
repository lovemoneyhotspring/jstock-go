# 案 5. 午後の第二の入口。前場リターン（11:30 終値 / 9:00 寄付 − 1）の下位 N=3 を 12:30 で買い（上位 N=3 を売り）、15:20 で手仕舞う。分足 2024-09〜。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import glob
c = con()
files = sorted(glob.glob(f'{ROOT}/equities_bars_minute/*.parquet'))
parts = []
for f in files:
    parts.append(c.execute(f"""
WITH m AS (SELECT Date, Code, Time, TRY_CAST(O AS DOUBLE) O, TRY_CAST(C AS DOUBLE) C FROM read_parquet('{f}') WHERE TRY_CAST(Vo AS DOUBLE) > 0)
SELECT Date, Code,
  arg_min(O, Time) FILTER (WHERE Time >= '09:00') AS o900,
  arg_max(C, Time) FILTER (WHERE Time <= '11:30') AS c1130,
  arg_min(O, Time) FILTER (WHERE Time >= '12:30') AS o1230,
  arg_min(Time, Time) FILTER (WHERE Time >= '12:30') AS t1230,
  arg_min(O, Time) FILTER (WHERE Time >= '15:20') AS o1520,
  count(*) FILTER (WHERE Time < '11:30') AS n_am
FROM m GROUP BY Date, Code""").df())
    if len(parts) % 50 == 0: print(len(parts), flush=True)
m = pd.concat(parts, ignore_index=True); del parts
m['Date'] = pd.to_datetime(m.Date); m = m.dropna(subset=['o900', 'c1130', 'o1230', 'o1520'])
m = m[m.t1230 <= '12:35']   # 後場の寄りが 5 分以内に付いた銘柄だけ
print('rows', len(m), m.Date.min().date(), m.Date.max().date(), flush=True)
# 母集団: 売買代金 20 日中央値 3 億以上、時価総額 100 億以上、決算の反応日を除く、貸借（売り用）
u = c.execute("""
SELECT Code, Date, MktCap, median(Va) OVER (PARTITION BY Code ORDER BY Date ROWS BETWEEN 20 PRECEDING AND 1 PRECEDING) va20
FROM bars WHERE Date >= '2024-08-01' AND AdjC > 0""").df(); u['Date'] = pd.to_datetime(u.Date)
ev = pd.read_parquet(f'{OUT}/p1_events.parquet')[['Code', 'rdate']].rename(columns={'rdate': 'Date'}); ev['earn'] = True
mg = c.execute("SELECT Date, Code, TRUE AS mrgn FROM master WHERE Mrgn='2' AND Date >= '2024-08-01'").df(); mg['Date'] = pd.to_datetime(mg.Date)
m = m.merge(u, on=['Code', 'Date']).merge(ev, on=['Code', 'Date'], how='left').merge(mg, on=['Code', 'Date'], how='left')
m = m[(m.va20 >= 3e8) & (m.MktCap >= 10000) & m.earn.isna()].copy(); m['mrgn'] = m.mrgn.fillna(False)
m['am'] = m.c1130 / m.o900 - 1; m['pm'] = m.o1520 / m.o1230 - 1
m['half'] = np.where(m.Date < '2025-09-01', 'H1', 'H2')
COST = 0.00057 + 0.001
print('universe rows', len(m), '銘柄/日', round(m.groupby('Date').size().mean()))
rows = []
print("\n== 横断: 前場リターン十分位（日ごと）→ 後場 12:30→15:20 の買いリターン bp（費用込み）")
m['dec'] = m.groupby('Date').am.transform(lambda s: pd.qcut(s.rank(method='first'), 10, labels=False) + 1 if len(s) >= 50 else np.nan)
for half in ['H1', 'H2']:
    x = m[m.half == half]; line = f"  {half}"
    for d in [1, 2, 5, 9, 10]:
        r = x[x.dec == d].pm - COST
        line += f"  D{d} {r.mean()*1e4:+5.0f}(t{r.mean()/r.std()*np.sqrt(len(r)):+.1f})"
        rows.append(dict(kind='xsec', half=half, dec=d, n=len(r), bp=r.mean()*1e4, t=r.mean()/r.std()*np.sqrt(len(r))))
    print(line)
dt = pd.read_csv(f'{OUT}/trades_10y.csv'); dt['date'] = pd.to_datetime(dt.date); dtl = dt[dt.side == 'long'].groupby('date').pnl.sum()
def run(sel, sgn, n, asc, label):
    d = m[sel].copy(); d = d.sort_values(['Date', 'am'], ascending=[True, asc]).groupby('Date').head(n)
    d['shares'] = np.floor(1e6 / (d.o1230 * 100)) * 100; d = d[d.shares > 0]
    d['pnl'] = sgn * (d.o1520 - d.o1230) * d.shares - COST * d.o1230 * d.shares
    p = d.groupby('Date').pnl.sum()
    r = sgn * d.pm - COST
    out = f"  {label:28s} 取引 {len(d):4d}  bp {r.mean()*1e4:+5.0f} (t{r.mean()/r.std()*np.sqrt(len(r)):+.1f})  損益 {p.sum():>11,.0f} 円"
    for half in ['H1', 'H2']:
        q = p[(p.index < '2025-09-01') if half == 'H1' else (p.index >= '2025-09-01')]
        eq = q.cumsum(); sh = q.mean() / q.std() * np.sqrt(245) if len(q) > 1 else np.nan
        out += f" | {half} 損益 {q.sum():>10,.0f} Sharpe {sh:5.2f} DD {(eq-eq.cummax()).min():>10,.0f}"
        rows.append(dict(kind='basket', label=label, half=half, pnl=q.sum(), sharpe=sh, dd=(eq - eq.cummax()).min(), trades=len(d)))
    out += f" | ロング脚相関 {p.reindex(dtl.index).fillna(0).corr(dtl):+.2f}"
    print(out)
print("\n== 籠: N=3・1 注文 100 万円、12:30 建て 15:20 手仕舞い")
run(m.am <= -0.03, 1, 3, True, '前場 ≤ −3% 買い（逆張り）')
run(m.am <= -0.05, 1, 3, True, '前場 ≤ −5% 買い（逆張り）')
run(m.dec == 1, 1, 3, True, '前場 最下位 3 買い')
run((m.am >= 0.03) & m.mrgn, -1, 3, False, '前場 ≥ +3% 売り（逆張り）')
run((m.am >= 0.05) & m.mrgn, -1, 3, False, '前場 ≥ +5% 売り（逆張り）')
run(m.am >= 0.03, 1, 3, False, '参考: 前場 ≥ +3% 買い（順張り）')
run(m.am <= -0.03, -1, 3, True, '参考: 前場 ≤ −3% 売り（順張り）')
pd.DataFrame(rows).to_csv(f'{OUT}/p5_afternoon.csv', index=False)
