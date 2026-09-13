# N5b. 信用の規制イベントを束ねた売りの籠。翌寄りで空売り、H 日後の寄りで買い戻し、TOPIX で日次ヘッジ。
# 実際に制度信用で売れる銘柄（貸借銘柄 Mrgn='2'）に限る。1 事象 = 資金の 1/K、枠が空いていなければ見送り。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

H, K, COST = 10, 20, 31.4      # 保有 10 日、枠 20、往復費用（個別 + TOPIX ヘッジ）
c = con()
c.execute(f"CREATE VIEW ma AS SELECT * FROM read_parquet('{ROOT}/markets_margin_alert/*.parquet', union_by_name=true)")
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, TRY_CAST(Va AS DOUBLE) Va FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
V = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)
OP = O.values; tpo = tp.open.values; ADV = V.rolling(20).mean().values
ELIG = ADV >= 1e8

# --- 貸借銘柄（制度信用で売れる）の時点表 ---
mm = c.execute("SELECT Date, Code, Mrgn FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
mm['Date'] = pd.to_datetime(mm.Date)
LEND = np.zeros((T, len(codes)), bool)
snap = {d: set(g.loc[g.Mrgn == '2', 'Code']) for d, g in mm.groupby('Date')}
sdays = sorted(snap); si = 0; cur = set()
for t, d in enumerate(tdays):
    while si < len(sdays) and sdays[si] <= d: cur = snap[sdays[si]]; si += 1
    if cur: LEND[t, [cpos[x] for x in cur if x in cpos]] = True
print('貸借銘柄 銘柄日', LEND.sum())

FLAGS = ['Restricted', 'DailyPublication', 'RestrictedByJSF', 'PrecautionByJSF']
raw = c.execute("SELECT PubDate, Code, PubReason FROM ma").df()
raw['PubDate'] = pd.to_datetime(raw.PubDate)
for f in FLAGS:
    raw[f] = raw.PubReason.str.extract(f'["\']{f}["\']: ["\'](\\d)', expand=False).astype(float)
pubdays = np.array(sorted(raw.PubDate.unique())); pidx = {d: i for i, d in enumerate(pubdays)}
tpos = {d: i for i, d in enumerate(tdays)}

def transitions(f):
    d = raw.loc[raw[f] == 1, ['Code', 'PubDate']].drop_duplicates().sort_values(['Code', 'PubDate'])
    d['p'] = d.PubDate.map(pidx)
    d['prev'] = d.groupby('Code').p.shift(1); d['next'] = d.groupby('Code').p.shift(-1)
    on = d[d.prev.isna() | (d.p - d.prev > 1)][['Code', 'PubDate']]
    last = d[d['next'].isna() | (d['next'] - d.p > 1)].copy()
    last['off_p'] = (last.p + 1).clip(upper=len(pubdays) - 1)
    off = pd.DataFrame({'Code': last.Code.values, 'PubDate': pubdays[last.off_p.values.astype(int)]})
    return on, off

ev = []
for f in FLAGS:
    on, off = transitions(f)
    for kind, e in [('発動', on), ('解除', off)]:
        e = e.copy(); e['kind'] = f'{f} {kind}'; ev.append(e)
ev = pd.concat(ev)
ev['ti'] = ev.PubDate.map(tpos); ev['ci'] = ev.Code.map(cpos)
ev = ev.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
ok = ELIG[ev.ti.values, ev.ci.values] & LEND[ev.ti.values, ev.ci.values]
ev = ev[np.where(np.isnan(ok), False, ok).astype(bool)]
# 同じ銘柄・同じ日に複数の規制が重なったら 1 件にまとめる
ev = ev.drop_duplicates(subset=['Code', 'PubDate']).sort_values('PubDate')
yrs = (tdays[-1] - tdays[0]).days / 365
print(f'事象 {len(ev)} 件（貸借銘柄・売買代金 1 億円以上）  年 {len(ev)/yrs:.0f} 件')
print(f'売買代金 20 日平均の中央値 {np.nanmedian(ADV[ev.ti.values, ev.ci.values])/1e8:.1f} 億円')

# --- 枠を持った籠 ---
# 母集団（売買代金 1 億円以上）の等加重・寄り→寄りの日次リターン
UNI = np.full(T, np.nan)
for t in range(1, T):
    m = ELIG[t - 1] & ~np.isnan(OP[t - 1]) & ~np.isnan(OP[t])
    if m.sum() >= 50: UNI[t] = (OP[t, m] / OP[t - 1, m] - 1).mean()
UNI_CUM = np.nancumprod(1 + np.nan_to_num(UNI))

def run(H, K, cost=COST, hedge=True):
    free = np.zeros(T, int)      # 各日に埋まっている枠
    pnl = pd.Series(0.0, index=tdays); taken = 0
    for _, r in ev.iterrows():
        a, b = r.ti + 1, r.ti + 1 + H
        if b >= T: continue
        if free[a:b].max() >= K: continue
        x = OP[b, r.ci] / OP[a, r.ci] - 1
        if np.isnan(x): continue
        if hedge == 'uni':   h = UNI_CUM[b] / UNI_CUM[a] - 1
        elif hedge:          h = tpo[b] / tpo[a] - 1
        else:                h = 0
        y = -x + h - cost / 1e4
        free[a:b] += 1; taken += 1
        pnl.iloc[b] += y / K
    return pnl, taken, free

for H_ in (5, 10, 20):
    pnl, taken, free = run(H_, K)
    d, i, o = stats(pnl), stats(split(pnl)[0]), stats(split(pnl)[1])
    print(f'\n保有 {H_:>2} 日 / 枠 {K}  採用 {taken} 件（平均稼働 {free.mean()/K*100:.0f}%）')
    print(f'  全期間  {fmt(d)}')
    print(f'  IS      {fmt(i)}')
    print(f'  OOS     {fmt(o)}')

print('\n=== 母集団の等加重でヘッジ（TOPIX ではなく同じ流動性帯の籠を買う）===')
for H_ in (5, 10, 20):
    p3, tk, fr = run(H_, K, hedge='uni')
    print(f'  保有 {H_:>2} 日  採用 {tk} 件')
    print(f'    全期間  {fmt(stats(p3))}')
    print(f'    IS      {fmt(stats(split(p3)[0]))}')
    print(f'    OOS     {fmt(stats(split(p3)[1]))}')

pnl, taken, free = run(H, K, hedge='uni')
print(f'\n=== 参考: ヘッジ無し（生の売り）保有 {H} 日 ===')
p2, _, _ = run(H, K, hedge=False)
print(f'  全期間  {fmt(stats(p2))}')
print(f'  IS      {fmt(stats(split(p2)[0]))}')
print(f'  OOS     {fmt(stats(split(p2)[1]))}')
pnl.to_csv(f'{OUT}/n5b_pnl.csv')

# --- 年別と最悪の DD 区間 ---
eq = (1 + pnl).cumprod()
dd = eq / eq.cummax() - 1
print('\n=== 年別（ヘッジあり・保有 10 日）===')
yr = pnl.groupby(pnl.index.year).apply(lambda s: (1 + s).prod() - 1) * 100
mx = dd.groupby(dd.index.year).min() * 100
for y in yr.index: print(f'  {y}  {yr[y]:>7.1f}%   年内最大DD {mx[y]:>6.1f}%')
i = dd.idxmin()
peak = eq[:i].idxmax()
print(f'\n最悪の DD  {dd.min()*100:.1f}%   {peak.date()} → {i.date()}')
rec = eq[i:][eq[i:] >= eq[peak]]
print(f'  回復  {rec.index[0].date() if len(rec) else "未回復"}')
