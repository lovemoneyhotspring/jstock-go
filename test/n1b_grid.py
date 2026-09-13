# N1b. 業種内逆張りの (振り返り LOOK, 保有 HOLD) 格子。総額中立 D1−D10 の粗 bp・費用込み bp・t 値を IS/OOS で。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

NDEC = 10; COST = 31.4   # 両脚 × 往復 15.7bp
c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = c.execute("SELECT Date, Code, TRY_CAST(MktCap AS DOUBLE) MktCap FROM bars WHERE AdjC>0").df()
M['Date'] = pd.to_datetime(M.Date)
M = M.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays).reindex(columns=C.columns)
del px
codes = list(C.columns); T = len(tdays)

master = c.execute("SELECT Date, Code, S33 FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
master['Date'] = pd.to_datetime(master.Date)
msnap = {d: g.set_index('Code').S33 for d, g in master.groupby('Date')}
msnap_days = sorted(msnap)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
elig = pd.DataFrame(False, index=tdays, columns=codes)
sec = pd.DataFrame(np.nan, index=tdays, columns=codes, dtype=object)
for k in range(len(mends) - 1):
    me, nx = mends.iloc[k], mends.iloc[k + 1]
    prior = [d for d in msnap_days if d <= me]
    if not prior: continue
    s33 = msnap[prior[-1]]
    cap = M.loc[me]; cap = cap[cap.index.isin(s33.index)].dropna().sort_values(ascending=False).head(500)
    rows = (tdays > me) & (tdays <= nx)
    elig.loc[rows, cap.index] = True
    sec.loc[rows, cap.index] = s33.reindex(cap.index).values
ELIG = elig.values
allsec = sorted({v for v in pd.unique(sec.values.ravel()) if isinstance(v, str)})
S = np.full((T, len(codes)), -1, dtype=np.int16)
for i, s in enumerate(allsec): S[(sec == s).values] = i
NS = len(allsec); CL, OP = C.values, O.values
del sec, elig, M

def demean(x, mask, srow):
    out = np.full_like(x, np.nan)
    for s in range(NS):
        m = mask & (srow == s)
        if m.sum() < 5: continue
        out[m] = x[m] - x[m].mean()
    return out

def run(LOOK, HOLD):
    sig = np.full((T, len(codes)), np.nan); sig[LOOK:] = CL[LOOK:] / CL[:-LOOK] - 1
    fwd = np.full((T, len(codes)), np.nan); fwd[:T - HOLD - 1] = OP[HOLD + 1:] / OP[1:T - HOLD] - 1
    out = {}
    for t in range(LOOK, T - HOLD - 1, HOLD):          # 重複しない
        m = ELIG[t] & ~np.isnan(sig[t]) & ~np.isnan(fwd[t]) & (S[t] >= 0)
        if m.sum() < 100: continue
        x = demean(sig[t], m, S[t]); y = demean(fwd[t], m, S[t])
        idx = np.where(m & ~np.isnan(x) & ~np.isnan(y))[0]
        if len(idx) < 100: continue
        dec = np.minimum((pd.Series(x[idx]).rank(pct=True).values * NDEC).astype(int), NDEC - 1)
        lo, hi = y[idx][dec == 0], y[idx][dec == NDEC - 1]
        if len(lo) < 5 or len(hi) < 5: continue
        out[tdays[t]] = lo.mean() - hi.mean()
    return pd.Series(out)

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan

print('業種内 D1−D10（重複なし・1 回転あたり bp）  費用 31.4bp/回転')
print(f'{"LOOK":>4} {"HOLD":>4} | {"IS粗":>7} {"IS t":>6} {"IS純":>7} | {"OOS粗":>7} {"OOS t":>6} {"OOS純":>7} | {"年率純%":>7}')
for LOOK in (3, 5, 10, 20, 60):
    for HOLD in (5, 10, 20):
        s = run(LOOK, HOLD)
        i, o = s[s.index <= IS_END], s[s.index > IS_END]
        if len(i) < 10 or len(o) < 10: continue
        net = s.mean() * 1e4 - COST
        ann = net * (245 / HOLD) / 100
        print(f'{LOOK:>4} {HOLD:>4} | {i.mean()*1e4:7.2f} {tv(i):6.2f} {i.mean()*1e4-COST:7.2f} | '
              f'{o.mean()*1e4:7.2f} {tv(o):6.2f} {o.mean()*1e4-COST:7.2f} | {ann:7.2f}')
