# N7. 決算の「数字のファクト」。(A) 通期予想に対する累計営業利益の進捗率、(B) 会社予想そのものの改定（上方/下方修正）。
# どちらも開示の翌寄り建て、母集団（時価総額上位 500）の当日平均で中立化。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = c.execute("SELECT Date, Code, TRY_CAST(MktCap AS DOUBLE) MktCap FROM bars WHERE AdjC>0").df()
M['Date'] = pd.to_datetime(M.Date)
M = M.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays).reindex(columns=O.columns)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays); OP_ = O.values

master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date)
mset = master.groupby('Date').Code.apply(set); sdays = sorted(mset.index)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
ELIG = np.zeros((T, len(codes)), bool)
for k in range(len(mends) - 1):
    me, nx = mends.iloc[k], mends.iloc[k + 1]
    prior = [d for d in sdays if d <= me]
    if not prior: continue
    cap = M.loc[me]; cap = cap[cap.index.isin(mset[prior[-1]])].dropna().sort_values(ascending=False).head(500)
    r = np.where((tdays > me) & (tdays <= nx))[0]
    ELIG[np.ix_(r, [cpos[x] for x in cap.index])] = True

f = c.execute("""
  SELECT DiscDate AS d, Code, CurPerType, CurFYEn,
         TRY_CAST(OP AS DOUBLE) AS op, TRY_CAST(FOP AS DOUBLE) AS fop
  FROM fins WHERE DocType NOT LIKE '%Forecast%' OR DocType IS NULL
""").df()
f['d'] = pd.to_datetime(f.d)
f = f.dropna(subset=['op', 'fop'])
f = f[f.fop > 0]
tpos = {d: i for i, d in enumerate(tdays)}
f['ti'] = f.d.map(tpos); f['ci'] = f.Code.map(cpos)
f = f.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
f = f[ELIG[f.ti.values, f.ci.values]].sort_values(['Code', 'd'])
print('決算開示（母集団内）', len(f), '銘柄', f.Code.nunique(), f.d.min().date(), f.d.max().date())

def window(ti, ci, a, b):
    ia, ib = ti + a, ti + b
    ok = (ia >= 0) & (ib < T)
    r = np.full(len(ti), np.nan)
    r[ok] = OP_[ib[ok], ci[ok]] / OP_[ia[ok], ci[ok]] - 1
    base = {}
    for t in np.unique(ti[ok]):
        if t + b >= T: continue
        mm = ELIG[t] & ~np.isnan(OP_[t + a]) & ~np.isnan(OP_[t + b])
        if mm.sum() < 50: continue
        base[t] = (OP_[t + b, mm] / OP_[t + a, mm] - 1).mean()
    r[ok] -= np.array([base.get(t, np.nan) for t in ti[ok]])
    return r

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan
def rep(sub, label, H):
    g = sub.groupby('d').y.mean().dropna()
    if len(g) < 20: return
    i, o = g[g.index <= IS_END], g[g.index > IS_END]
    if len(i) < 10 or len(o) < 10: return
    print(f'{label:<26} {len(sub):>6} {sub.y.mean()*1e4:>8.1f} {tv(g):>6.2f} '
          f'{i.mean()*1e4:>8.1f} {tv(i):>6.2f} {o.mean()*1e4:>8.1f} {tv(o):>6.2f}')

# --- A: 進捗率（四半期の型ごとに、同じ開示月の母集団の中で十分位）---
PRG = {'1Q': 0.25, '2Q': 0.50, '3Q': 0.75}
a = f[f.CurPerType.isin(PRG)].copy()
a['prg'] = a.op / a.fop
a['norm'] = a.prg / a.CurPerType.map(PRG)          # 1.0 = 予想どおりのペース
a['cohort'] = a.CurPerType + '_' + a.d.dt.to_period('M').astype(str)
a = a[a.groupby('cohort').Code.transform('size') >= 50]
a['dec'] = a.groupby('cohort').norm.rank(pct=True).mul(5).astype(int).clip(0, 4)
print('\n進捗率の事象', len(a), ' コホート', a.cohort.nunique())
for H in (20, 60):
    a['y'] = window(a.ti.values, a.ci.values, 1, 1 + H)
    e = a.dropna(subset=['y'])
    print(f'\n=== A 進捗率 五分位（開示翌寄り → {H} 日、母集団中立 bp）===')
    print(f'{"群":<26} {"件数":>6} {"全期間":>8} {"t":>6} {"IS":>8} {"t":>6} {"OOS":>8} {"t":>6}')
    for q in range(5): rep(e[e.dec == q], f'Q{q+1}（Q5=進捗が最も良い）', H)
    lo, hi = e[e.dec == 0], e[e.dec == 4]
    g = hi.groupby('d').y.mean() - lo.groupby('d').y.mean()
    g = g.dropna()
    i, o = g[g.index <= IS_END], g[g.index > IS_END]
    print(f'{"Q5 − Q1":<26} {len(g):>6} {g.mean()*1e4:>8.1f} {tv(g):>6.2f} '
          f'{i.mean()*1e4:>8.1f} {tv(i):>6.2f} {o.mean()*1e4:>8.1f} {tv(o):>6.2f}')

# --- B: 会社予想の改定（同じ CurFYEn で FOP が前回から変わった）---
b = f.copy()
b['pfop'] = b.groupby(['Code', 'CurFYEn']).fop.shift(1)
b = b.dropna(subset=['pfop'])
b = b[b.pfop != 0]
b['rev'] = b.fop / b.pfop - 1
b = b[b.rev.abs() > 1e-6]
print(f'\n予想の改定 {len(b)} 件（上方 {int((b.rev>0).sum())} / 下方 {int((b.rev<0).sum())}）')
for H in (5, 20, 60):
    b['y'] = window(b.ti.values, b.ci.values, 1, 1 + H)
    e = b.dropna(subset=['y'])
    print(f'\n=== B 会社予想の改定（開示翌寄り → {H} 日、母集団中立 bp）===')
    print(f'{"群":<26} {"件数":>6} {"全期間":>8} {"t":>6} {"IS":>8} {"t":>6} {"OOS":>8} {"t":>6}')
    rep(e[e.rev > 0], '上方修正', H)
    rep(e[e.rev > 0.10], '  うち +10% 以上', H)
    rep(e[e.rev < 0], '下方修正', H)
    rep(e[e.rev < -0.10], '  うち −10% 以下', H)
