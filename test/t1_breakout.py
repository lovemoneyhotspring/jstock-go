# T1. ブレイクアウト ＋ ATR 追従。各月末の時価総額上位 500 を翌月の母集団とし、終値が直前 N 日高値を抜けた翌寄りで買い、
# 最高終値 − k×ATR(20) を割った翌寄りで売る。最大 30 枠、1 枠 = 1/30、片道 0.10%。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import itertools

COST = 0.001; SLOTS = 30
c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC, H, L, C AS Craw, AdjFactor, MktCap FROM bars WHERE AdjC>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date)
px = px[px.Date.isin(tdays)]
# 調整済み高値・安値: AdjC / C の比で H, L を調整
ratio = px.AdjC / px.Craw
px['AdjH'] = px.H * ratio; px['AdjL'] = px.L * ratio
def wide(col): return px.pivot(index='Date', columns='Code', values=col).reindex(tdays)
C = wide('AdjC'); O = wide('AdjO'); H = wide('AdjH'); L = wide('AdjL'); M = wide('MktCap')
del px
codes = list(C.columns); T = len(tdays)
print('wide', C.shape)

# --- 時点母集団: 月末の master ∩ 時価総額上位 500 → 翌月に有効 ---
master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date)
mset = master.groupby('Date').Code.apply(set)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
elig = pd.DataFrame(False, index=tdays, columns=codes)
for k in range(len(mends) - 1):
    me, nx = mends.iloc[k], mends.iloc[k + 1]
    prior = mset.index[mset.index <= me]
    if len(prior) == 0: continue
    mm = mset[prior[-1]]
    cap = M.loc[me]; cap = cap[cap.index.isin(mm)].dropna().sort_values(ascending=False).head(500)
    elig.loc[(elig.index > me) & (elig.index <= nx), cap.index] = True
ELIG = elig.values
print('eligible stock-days', ELIG.sum())

# --- ATR(20) ---
prevC = C.shift(1)
tr = pd.concat([H - L, (H - prevC).abs(), (L - prevC).abs()], axis=1, keys=['a', 'b', 'c']).T.groupby(level=1).max().T
ATR = tr.rolling(20).mean().values
CL = C.values; OP = O.values; HI = H.values
tpc = tp.close.values; tpo = tp.open.values

def breakout(N):
    hh = H.shift(1).rolling(N).max().values
    return (CL > hh) & ELIG & ~np.isnan(ATR)

# --- 補助の検定: ブレイクアウトの翌寄りから 20 / 60 日の超過 vs 母集団の全営業日 ---
def xsec(N):
    b = breakout(N)
    out = {}
    for h in [20, 60]:
        ent = np.roll(OP, -1, axis=0); ex = np.roll(CL, -1 - h, axis=0)
        r = ex / ent - 1
        tr_ = np.roll(tpc, -1 - h) / np.roll(tpo, -1) - 1
        exc = r - tr_[:, None]
        valid = ~np.isnan(exc); valid[-(h + 2):, :] = False
        for per, mask in [('IS', np.asarray(tdays <= IS_END)), ('OOS', np.asarray(tdays > IS_END))]:
            mm = valid & mask[:, None]
            e_b = exc[b & mm]; e_all = exc[ELIG & mm]
            out[(h, per)] = (e_b.mean() * 1e4, e_b.mean() / e_b.std() * np.sqrt(len(e_b)), len(e_b), e_all.mean() * 1e4)
    return out

print("\n== 補助の検定: ブレイクアウト後の超過（bp、t 値、件数）と母集団の全営業日の平均超過")
for N in [20, 55, 100]:
    o = xsec(N)
    for h in [20, 60]:
        print(f"  N{N:3d} {h}日  IS {o[(h,'IS')][0]:+6.0f}bp (t{o[(h,'IS')][1]:+.1f}, n={o[(h,'IS')][2]}) 全体 {o[(h,'IS')][3]:+.0f}  |  OOS {o[(h,'OOS')][0]:+6.0f}bp (t{o[(h,'OOS')][1]:+.1f}, n={o[(h,'OOS')][2]}) 全体 {o[(h,'OOS')][3]:+.0f}")

# --- 籠 ---
def simulate(N, k):
    b = breakout(N)
    daily = np.zeros(T); pos = {}   # j -> dict(entry_t, hc)
    pend_buy = []; pend_sell = []
    ntrades = 0; hold_days = []
    for t in range(1, T):
        r = 0.0; w = 1.0 / SLOTS
        # 寄りの売り
        for j in pend_sell:
            if j in pos:
                r += w * (OP[t, j] / CL[t - 1, j] - 1 - COST); hold_days.append(t - pos[j]['entry']); del pos[j]
        # 寄りの買い（枠が空いていれば）
        bought = []
        for j in pend_buy:
            if j in pos or len(pos) >= SLOTS or np.isnan(OP[t, j]) or np.isnan(CL[t, j]): continue
            pos[j] = dict(entry=t, hc=CL[t, j]); bought.append(j); ntrades += 1
            r += w * (CL[t, j] / OP[t, j] - 1 - COST)
        # 継続の日次
        for j, p in pos.items():
            if j in bought: continue
            cc = CL[t, j] / CL[t - 1, j] - 1
            if np.isnan(cc): cc = 0.0
            r += w * cc
            if not np.isnan(CL[t, j]): p['hc'] = max(p['hc'], CL[t, j])
        daily[t] = r
        # 引け後の判断
        # 母集団から外れても持ち続ける（時点母集団は入口だけに使う）。価格が消えた銘柄は翌寄りで手仕舞い
        pend_sell = [j for j, p in pos.items() if np.isnan(CL[t, j]) or CL[t, j] < p['hc'] - k * ATR[t, j]]
        cand = np.where(b[t])[0]
        if len(cand): cand = cand[np.argsort(-np.nan_to_num(M.values[t, cand], nan=0))]
        pend_buy = list(cand)
    s = pd.Series(daily, index=tdays)
    return s[s.index >= tdays[260]], ntrades, np.mean(hold_days) if hold_days else np.nan

rows = []; grid = []
tpr = tp.close.pct_change()
print("\n== 籠（最大 30 枠、片道 0.10%）")
for N, k in itertools.product([20, 55, 100], [3, 4, 5]):
    s, nt, hd = simulate(N, k)
    a, o = split(s); sa, so = stats(a), stats(o); lab = f"N{N} k{k}"
    print(f"  {lab:9s} IS {fmt(sa)} | OOS {fmt(so)}  取引 {nt}  平均保有 {hd:.0f}日")
    grid.append(dict(label=lab, is_cagr=sa['cagr'], is_dd=sa['dd'], oos_cagr=so['cagr'], oos_dd=so['dd']))
    for per, x in [('IS', sa), ('OOS', so)]: rows.append(dict(strategy=lab, period=per, trades=nt, hold=hd, **x))
ta, to = split(tpr[tpr.index >= tdays[260]])
print(f"  {'TOPIX':9s} IS {fmt(stats(ta))} | OOS {fmt(stats(to))}")
for per, x in [('IS', stats(ta)), ('OOS', stats(to))]: rows.append(dict(strategy='TOPIX', period=per, trades=0, hold=0, **x))
g = pd.DataFrame(grid)
print(f"  格子 9 の中央値: IS CAGR {g.is_cagr.median():.2f}% DD {g.is_dd.median():.1f}% | OOS CAGR {g.oos_cagr.median():.2f}% DD {g.oos_dd.median():.1f}%")
pd.DataFrame(rows).to_csv(f'{OUT}/t1_breakout.csv', index=False)
print('saved')
