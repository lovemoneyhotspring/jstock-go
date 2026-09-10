# B. 横断モメンタム籠 ＋ ボラ目標。各月末に時価総額上位 500 から 12-1 か月リターン上位 N を等金額、翌営業日の寄付で入れ替え。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import itertools

COST = 0.001; SPREAD = 0.01; THRESH = 0.05
c = con()
tp = topix(c); tdays = tp.index
rf = irx()

# --- 価格を wide に ---
px = c.execute("SELECT Date, Code, AdjO, AdjC, MktCap FROM bars WHERE AdjC>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date)
px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
M = px.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays)
del px
print('wide', C.shape)

# --- 月末と母集団 ---
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
mpos = {d: i for i, d in enumerate(tdays)}
master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date)
mset = master.groupby('Date').Code.apply(set)

R = C.pct_change()             # 終値ベースの日次
DAILY = R.values; OPEN = O.values; CLOSE = C.values
codes = list(C.columns); cidx = {k: i for i, k in enumerate(codes)}

def rank_at(me):
    """月末 me の信号: 12-1 か月リターン。母集団は時点の master ∩ 時価総額上位 500、13 か月以上の価格"""
    i = mpos[me]
    if i < 260: return None
    mm = mset.get(me)
    if mm is None:
        # master が無い日は直近の日
        prior = mset.index[mset.index <= me]
        if len(prior) == 0: return None
        mm = mset[prior[-1]]
    cap = M.iloc[i]
    cap = cap[cap.index.isin(mm)].dropna().sort_values(ascending=False).head(500)
    c0 = C.iloc[i - 21]; c12 = C.iloc[i - 252]
    sig = (c0 / c12 - 1).reindex(cap.index).dropna()
    return sig

# --- 十分位の月次超過（横断の検定、費用なし、月末終値→月末終値） ---
dec_rows = []
melist = list(mends.values)
for k in range(len(melist) - 1):
    me, nx = melist[k], melist[k + 1]
    sig = rank_at(me)
    if sig is None or len(sig) < 100: continue
    ret = (C.loc[nx] / C.loc[me] - 1).reindex(sig.index)
    tret = tp.close[nx] / tp.close[me] - 1
    dec = pd.qcut(sig.rank(method='first'), 10, labels=False) + 1
    for d in range(1, 11):
        dec_rows.append(dict(me=me, dec=d, ex=ret[dec == d].mean() - tret))
dd = pd.DataFrame(dec_rows); dd['period'] = np.where(dd.me <= IS_END, 'IS', 'OOS')
print("\n== 十分位の月次超過（等加重 − TOPIX、bp/月、t 値）")
for per in ['IS', 'OOS']:
    x = dd[dd.period == per]; line = []
    for d in [1, 2, 5, 9, 10]:
        y = x[x.dec == d].ex; line.append(f"D{d} {y.mean()*1e4:+5.0f}(t{y.mean()/y.std()*np.sqrt(len(y)):+.1f})")
    y10 = x[x.dec == 10].ex.values; y1 = x[x.dec == 1].ex.values
    print(f"  {per:3s} n={len(y10)}月 " + '  '.join(line) + f"  D10-D1 {(y10-y1).mean()*1e4:+.0f}bp (t{(y10-y1).mean()/(y10-y1).std()*np.sqrt(len(y10)):+.1f})")
dd.to_csv(f'{OUT}/b_deciles.csv', index=False)

# --- 籠の日次リターン（翌営業日の寄付で入れ替え、片道 0.10%） ---
def basket_daily(N):
    daily = np.full(len(tdays), np.nan)
    hold = None  # 現在の銘柄の列番号
    events = {}
    for me in melist:
        sig = rank_at(me)
        if sig is None or len(sig) < 100: continue
        top = sig.sort_values(ascending=False).head(N).index
        r = mpos[me] + 1
        if r < len(tdays): events[r] = np.array([cidx[k] for k in top])
    first = min(events)
    for t in range(first, len(tdays)):
        if t in events:
            new = events[t]
            if hold is None:
                daily[t] = np.nanmean(CLOSE[t, new] / OPEN[t, new] - 1) - COST
            else:
                stay = np.isin(hold, new)
                cc = CLOSE[t, hold] / CLOSE[t - 1, hold] - 1                      # 継続銘柄
                ex = OPEN[t, hold] / CLOSE[t - 1, hold] - 1 - COST                # 入替銘柄: 前日引け→寄りで売る
                fresh = new[~np.isin(new, hold)]
                nw = CLOSE[t, fresh] / OPEN[t, fresh] - 1 - COST                  # 新規: 寄りで買い→引け
                nw_mean = np.nanmean(nw) if len(fresh) else 0.0
                slot = np.where(stay, cc, (1 + ex) * (1 + nw_mean) - 1)           # 売却代金で新規を買う
                daily[t] = np.nanmean(slot)
            hold = new
        else:
            daily[t] = np.nanmean(DAILY[t, hold])
    return pd.Series(daily, index=tdays).dropna()

def voltarget(r, target, cap=1.5, floor=0.5, win=20):
    vol = r.rolling(win).std() * np.sqrt(245)
    want = (target / vol).clip(floor, cap)
    e = np.empty(len(want)); t = np.zeros(len(want)); cur = np.nan
    for i, w in enumerate(want.values):
        if np.isnan(w): e[i] = np.nan; continue
        if np.isnan(cur) or abs(w - cur) >= THRESH: t[i] = abs(w - (0 if np.isnan(cur) else cur)); cur = w
        e[i] = cur
    exp = pd.Series(e, index=r.index).shift(1)
    borrow = (exp - 1).clip(lower=0) * (rf.reindex(r.index).ffill().fillna(0) + SPREAD) / 245
    return (exp * r - borrow - pd.Series(t, index=r.index).shift(1).fillna(0) * COST).dropna()

rows = []; grid = []
tpr = tp.close.pct_change()
print("\n== 籠（翌営業日寄付で入替、片道 0.10%）")
for N in [30, 50, 100]:
    b = basket_daily(N)
    for vt in [None, 0.12, 0.15]:
        s = b if vt is None else voltarget(b, vt)
        s = s[s.index >= b.index[0] + pd.Timedelta(days=45)]
        a, o = split(s); label = f"N{N} vt{'none' if vt is None else int(vt*100)}"
        sa, so = stats(a), stats(o)
        print(f"  {label:12s} IS {fmt(sa)} | OOS {fmt(so)}")
        grid.append(dict(label=label, is_cagr=sa['cagr'], is_dd=sa['dd'], oos_cagr=so['cagr'], oos_dd=so['dd']))
        for per, x in [('IS', sa), ('OOS', so)]: rows.append(dict(strategy=label, period=per, **x))
    if N == 30: base_index = s.index
ta, to = split(tpr.reindex(base_index).dropna())
print(f"  {'TOPIX':12s} IS {fmt(stats(ta))} | OOS {fmt(stats(to))}")
for per, x in [('IS', stats(ta)), ('OOS', stats(to))]: rows.append(dict(strategy='TOPIX', period=per, **x))
g = pd.DataFrame(grid)
print(f"  格子 9 通りの中央値: IS CAGR {g.is_cagr.median():.2f}% DD {g.is_dd.median():.1f}% | OOS CAGR {g.oos_cagr.median():.2f}% DD {g.oos_dd.median():.1f}%")
pd.DataFrame(rows).to_csv(f'{OUT}/b_momentum.csv', index=False)
print('saved')
