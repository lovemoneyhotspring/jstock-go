# N1. 業種内ペア逆張りの横断検定。親ノート: vault/20-research/2026-09-jp-low-dd-plan.md
# 各月末の時価総額上位 500 を翌月の母集団とし、S33 業種内で「直近 5 日リターンの業種内偏差」の十分位を作り、
# 翌寄り建て・5 営業日後の寄り決めの「業種内偏差リターン」を測る。t 値は重複しない 5 日刻みのコホートで。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

LOOK, HOLD, NDEC = 5, 5, 10
c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date)
px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = c.execute("SELECT Date, Code, TRY_CAST(MktCap AS DOUBLE) MktCap FROM bars WHERE AdjC>0").df()
M['Date'] = pd.to_datetime(M.Date)
M = M.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays).reindex(columns=C.columns)
del px
codes = list(C.columns); T = len(tdays)
print('wide', C.shape)

# --- 時点母集団と業種: 月末の master ∩ 時価総額上位 500 → 翌月に有効 ---
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
print('eligible stock-days', ELIG.sum())

# --- 業種コードを整数に落とす（NaN は -1） ---
allsec = sorted({v for v in pd.unique(sec.values.ravel()) if isinstance(v, str)})
smap = {s: i for i, s in enumerate(allsec)}
S = np.full((T, len(codes)), -1, dtype=np.int16)
for i, s in enumerate(allsec):
    S[(sec == s).values] = i
NS = len(allsec)
print('sectors', NS)

CL, OP = C.values, O.values

def sector_demean(x, mask, srow):
    """行ベクトル x を業種平均からの偏差にする。mask は有効な要素。"""
    out = np.full_like(x, np.nan)
    for s in range(NS):
        m = mask & (srow == s)
        if m.sum() < 5: continue
        out[m] = x[m] - x[m].mean()
    return out

# --- 信号（t 日の引け時点）と将来（t+1 寄り → t+1+HOLD 寄り）---
sig_r = np.full((T, len(codes)), np.nan)
sig_r[LOOK:] = CL[LOOK:] / CL[:-LOOK] - 1
fwd = np.full((T, len(codes)), np.nan)
fwd[:T - HOLD - 1] = OP[HOLD + 1:] / OP[1:T - HOLD] - 1

rows = []
for t in range(LOOK, T - HOLD - 1):
    m = ELIG[t] & ~np.isnan(sig_r[t]) & ~np.isnan(fwd[t]) & (S[t] >= 0)
    if m.sum() < 100: continue
    x = sector_demean(sig_r[t], m, S[t])
    y = sector_demean(fwd[t], m, S[t])
    idx = np.where(m & ~np.isnan(x) & ~np.isnan(y))[0]
    if len(idx) < 100: continue
    rk = pd.Series(x[idx]).rank(pct=True).values
    dec = np.minimum((rk * NDEC).astype(int), NDEC - 1)
    rows.append((t, idx, dec, y[idx]))
print('signal days', len(rows))

# --- 十分位の平均（全日、記述統計）---
def table(sel):
    acc = [[] for _ in range(NDEC)]
    for t, idx, dec, y in rows:
        if not sel(tdays[t]): continue
        for d in range(NDEC):
            k = dec == d
            if k.any(): acc[d].append(y[k].mean())
    return [np.mean(a) * 1e4 if a else np.nan for a in acc]

is_t = table(lambda d: d <= IS_END); oos_t = table(lambda d: d > IS_END)
print('\n十分位（業種内偏差、翌寄り→5日後寄り、bp）  D1=直近5日で最も下げた')
print('  D    IS      OOS')
for d in range(NDEC):
    print(f'  D{d+1:<2} {is_t[d]:7.1f} {oos_t[d]:7.1f}')

# --- 重複しない 5 日刻みのコホートで D1−D10 スプレッド ---
def spread_series(sel):
    out = {}
    for t, idx, dec, y in rows:
        if t % HOLD: continue          # 重複しない
        if not sel(tdays[t]): continue
        lo, hi = y[dec == 0], y[dec == NDEC - 1]
        if len(lo) < 5 or len(hi) < 5: continue
        out[tdays[t]] = lo.mean() - hi.mean()
    return pd.Series(out)

def ttest(s):
    return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))), 0
print('\nD1−D10 スプレッド（重複なし 5 日刻み、両建て金額中立の 1 回転あたり）')
for name, sel in [('IS ', lambda d: d <= IS_END), ('OOS', lambda d: d > IS_END)]:
    s = spread_series(sel)
    t_, p = ttest(s)
    print(f'  {name}  n={len(s):4d}  平均 {s.mean()*1e4:7.2f} bp  t={t_:6.2f}  '
          f'勝率 {(s>0).mean()*100:5.1f}%  費用込み {(s.mean()*1e4-31.4):7.2f} bp')

s_all = spread_series(lambda d: True)
s_all.to_csv(f'{OUT}/n1_spread.csv')
print(f'\n往復費用: 両脚とも建てて閉じるので 15.7bp × 2 = 31.4bp/回転')
