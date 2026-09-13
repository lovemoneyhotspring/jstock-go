# N3. 大量保有報告（EDINET）の提出後。提出日 t の保有比率の変化 Δ を事象とし、翌寄り → H 日後寄りの
# 母集団中立リターンを測る。データは 2021-07 から（IS/OOS の分割点は共通の 2022-12-31）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
c.execute(f"CREATE VIEW lv AS SELECT * FROM read_parquet('{ROOT}/edinet_large_volume_shareholders/*.parquet', union_by_name=true)")
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = c.execute("SELECT Date, Code, TRY_CAST(MktCap AS DOUBLE) MktCap FROM bars WHERE AdjC>0").df()
M['Date'] = pd.to_datetime(M.Date)
M = M.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays).reindex(columns=O.columns)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)

master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date)
mset = master.groupby('Date').Code.apply(set); msnap_days = sorted(mset.index)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
ELIG = np.zeros((T, len(codes)), bool)
for k in range(len(mends) - 1):
    me, nx = mends.iloc[k], mends.iloc[k + 1]
    prior = [d for d in msnap_days if d <= me]
    if not prior: continue
    cap = M.loc[me]; cap = cap[cap.index.isin(mset[prior[-1]])].dropna().sort_values(ascending=False).head(500)
    r = np.where((tdays > me) & (tdays <= nx))[0]
    ELIG[np.ix_(r, [cpos[x] for x in cap.index])] = True

ev = c.execute("""
  SELECT SubDate AS d, Code, DocTypeCode,
         TRY_CAST(TotalShsRatio AS DOUBLE) AS ratio,
         TRY_CAST(TotalShsRatio AS DOUBLE) - COALESCE(TRY_CAST(TotalShsRatioLast AS DOUBLE), 0) AS delta
  FROM lv WHERE Code IS NOT NULL
""").df()
ev['d'] = pd.to_datetime(ev.d)
tpos = {d: i for i, d in enumerate(tdays)}
ev['ti'] = ev.d.map(tpos); ev['ci'] = ev.Code.map(cpos)
ev = ev.dropna(subset=['ti', 'ci', 'delta']).astype({'ti': int, 'ci': int})
ev = ev[ELIG[ev.ti.values, ev.ci.values]]
print('事象（母集団内）', len(ev), '銘柄', ev.Code.nunique(), '期間', ev.d.min().date(), ev.d.max().date())

OP = O.values
def fwd(ti, ci, H):
    a, b = ti + 1, ti + 1 + H
    ok = b < T
    r = np.full(len(ti), np.nan)
    r[ok] = OP[b[ok], ci[ok]] / OP[a[ok], ci[ok]] - 1
    base = {}
    for t in np.unique(ti[ok]):
        if t + 1 + H >= T: continue
        m = ELIG[t] & ~np.isnan(OP[t + 1]) & ~np.isnan(OP[t + 1 + H])
        base[t] = (OP[t + 1 + H, m] / OP[t + 1, m] - 1).mean()
    r[ok] -= np.array([base.get(t, np.nan) for t in ti[ok]])
    return r

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan
def report(sub):
    g = sub.groupby('d').y.mean().dropna()
    if len(g) < 20: return None
    i, o = g[g.index <= IS_END], g[g.index > IS_END]
    if len(i) < 20 or len(o) < 20: return None
    return len(sub), i.mean()*1e4, tv(i), o.mean()*1e4, tv(o)

for H in (5, 10, 20, 60):
    ev['y'] = fwd(ev.ti.values, ev.ci.values, H)
    e = ev.dropna(subset=['y'])
    print(f'\n=== 保有 {H} 日（翌寄り → {H} 日後寄り、母集団中立 bp）===')
    print(f'{"群":<26} {"件数":>7} {"IS bp":>8} {"IS t":>6} {"OOS bp":>8} {"OOS t":>6}')
    for label, sub in [
        ('新規取得（変更前 0）',   e[(e.delta == e.ratio)]),
        ('買い増し Δ>0',          e[e.delta > 0]),
        ('  うち Δ>+1pt',         e[e.delta > 1]),
        ('売却 Δ<0',              e[e.delta < 0]),
        ('  うち Δ<−1pt',         e[e.delta < -1]),
        ('全事象',                e)]:
        r = report(sub)
        if r: print(f'{label:<26} {r[0]:>7} {r[1]:>8.1f} {r[2]:>6.2f} {r[3]:>8.1f} {r[4]:>6.2f}')
