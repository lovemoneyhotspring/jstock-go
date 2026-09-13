# N2. 空売り残高の増減に追随できるか。開示日 t に出た報告の合計残高比率の変化 Δ を事象とし、
# 翌寄り → H 日後寄りの TOPIX 超過を測る。t 値は日でクラスタ（日ごとの平均を系列にする）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
c.execute(f"CREATE VIEW ss AS SELECT * FROM read_parquet('{ROOT}/markets_short_sale_report/*.parquet', union_by_name=true)")
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = c.execute("SELECT Date, Code, TRY_CAST(MktCap AS DOUBLE) MktCap FROM bars WHERE AdjC>0").df()
M['Date'] = pd.to_datetime(M.Date)
M = M.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays).reindex(columns=C.columns)
del px
codes = list(C.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)

# --- 時点母集団（プライム相当 × 時価総額上位 500）---
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
    ci = [cpos[x] for x in cap.index]
    ELIG[np.ix_(r, ci)] = True

# --- 事象: 開示日ごとの合計残高比率の変化 ---
ev = c.execute("""
  SELECT DiscDate AS d, Code,
         SUM(TRY_CAST(ShrtPosToSO AS DOUBLE)) - SUM(COALESCE(TRY_CAST(PrevRptRatio AS DOUBLE), 0)) AS delta,
         SUM(TRY_CAST(ShrtPosToSO AS DOUBLE)) AS level, COUNT(*) AS n
  FROM ss GROUP BY 1, 2 HAVING delta IS NOT NULL
""").df()
ev['d'] = pd.to_datetime(ev.d)
tpos = {d: i for i, d in enumerate(tdays)}
ev['ti'] = ev.d.map(tpos); ev['ci'] = ev.Code.map(cpos)
ev = ev.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
ev = ev[ELIG[ev.ti.values, ev.ci.values]]
print('事象（母集団内）', len(ev), '銘柄', ev.Code.nunique(), '期間', ev.d.min().date(), ev.d.max().date())

OP = O.values; tpo = tp.open.values

UNI = np.where(ELIG, OP, np.nan)

def fwd(ti, ci, H):
    """母集団の当日平均で中立化した H 日リターン（母集団は t 日に有効な銘柄）"""
    a, b = ti + 1, ti + 1 + H
    ok = b < T
    r = np.full(len(ti), np.nan)
    r[ok] = OP[b[ok], ci[ok]] / OP[a[ok], ci[ok]] - 1
    # 母集団平均（t ごとに一度だけ計算）
    base = {}
    for t in np.unique(ti[ok]):
        if t + 1 + H >= T: continue
        m = ELIG[t] & ~np.isnan(OP[t + 1]) & ~np.isnan(OP[t + 1 + H])
        base[t] = (OP[t + 1 + H, m] / OP[t + 1, m] - 1).mean()
    r[ok] -= np.array([base.get(t, np.nan) for t in ti[ok]])
    return r

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan

def report(sub, label, H):
    """日でクラスタした平均 bp と t"""
    g = sub.groupby('d').y.mean().dropna()
    if len(g) < 20: return None
    i, o = g[g.index <= IS_END], g[g.index > IS_END]
    return (label, len(sub), i.mean()*1e4, tv(i), o.mean()*1e4, tv(o))

for H in (5, 10, 20):
    ev['y'] = fwd(ev.ti.values, ev.ci.values, H)
    e = ev.dropna(subset=['y'])
    # 母集団の平均（同じ日に有効な全銘柄）を引いた「事象の超過」ではなく TOPIX 超過のまま扱う
    print(f'\n=== 保有 {H} 日（翌寄り → {H} 日後寄り、母集団中立 bp）===')
    print(f'{"群":<26} {"件数":>7} {"IS bp":>8} {"IS t":>6} {"OOS bp":>8} {"OOS t":>6}')
    bands = [
        ('残高が減少 Δ<0',        e[e.delta < 0]),
        ('  うち Δ<−0.5pt',      e[e.delta < -0.005]),
        ('  うち Δ<−1.0pt',      e[e.delta < -0.010]),
        ('残高が増加 Δ>0',        e[e.delta > 0]),
        ('  うち Δ>+0.5pt',      e[e.delta > 0.005]),
        ('  うち Δ>+1.0pt',      e[e.delta > 0.010]),
        ('全事象',                e),
    ]
    for label, sub in bands:
        r = report(sub, label, H)
        if r: print(f'{r[0]:<26} {r[1]:>7} {r[2]:>8.1f} {r[3]:>6.2f} {r[4]:>8.1f} {r[5]:>6.2f}')
