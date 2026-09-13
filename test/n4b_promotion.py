# N4b. 市場区分の昇格（→ 東証一部 / プライム）。実施日の −5 日に跳ね（発表と推定）、−4 日に持ち越しの効き。
# 「発表の翌寄りで買い、−1 日の寄りで売る」を事象単位と籠で測る。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, TRY_CAST(Va AS DOUBLE) Va FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
V = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)
OP = O.values; tpo = tp.open.values
ADV = V.rolling(20).mean().values
ELIG = ADV >= 1e8

m = c.execute("SELECT Date, Code, Mkt FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
m['Date'] = pd.to_datetime(m.Date); m = m.sort_values(['Code', 'Date'])
m['pMkt'] = m.groupby('Code').Mkt.shift(1)
PRIME = {'0101', '0111'}
up = m[(m.Mkt != m.pMkt) & m.pMkt.notna() & m.Mkt.isin(PRIME) & ~m.pMkt.isin(PRIME)].copy()
tpos = {d: i for i, d in enumerate(tdays)}
up['ti'] = up.Date.map(tpos); up['ci'] = up.Code.map(cpos)
up = up.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
ok = ELIG[up.ti.values, up.ci.values]
up = up[np.where(np.isnan(ok), False, ok).astype(bool)]
up['adv'] = ADV[up.ti.values, up.ci.values]
print('昇格 事象', len(up), '  年あたり', round(len(up) / ((tdays[-1] - tdays[0]).days / 365), 1))

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan

# --- 事象単位: 建て −4 寄り、決め e 寄り。TOPIX でヘッジ（β=1 と仮定） ---
print(f'\n{"建て→決め":<14} {"件数":>5} {"粗bp":>8} {"純bp":>8} {"t":>6} {"IS純":>8} {"OOS純":>8} {"勝率":>6}')
COST = 15.7 * 2   # 個別の往復 15.7bp ×2（ヘッジの TOPIX 側も建て・決め）
best = None
for a, b in [(-4, -3), (-4, -2), (-4, -1), (-4, 0), (-4, 2), (-4, 5)]:
    ia, ib = up.ti.values + a, up.ti.values + b
    ok2 = (ia >= 0) & (ib < T)
    r = np.full(len(up), np.nan)
    r[ok2] = (OP[ib[ok2], up.ci.values[ok2]] / OP[ia[ok2], up.ci.values[ok2]] - 1) - (tpo[ib[ok2]] / tpo[ia[ok2]] - 1)
    s = pd.Series(r, index=up.Date.values).dropna()
    net = s * 1e4 - COST
    i, o = net[net.index <= IS_END], net[net.index > IS_END]
    print(f'{f"−4 → {b:+d}":<14} {len(s):>5} {s.mean()*1e4:>8.1f} {net.mean():>8.1f} {tv(net):>6.2f} '
          f'{i.mean():>8.1f} {o.mean():>8.1f} {(net>0).mean()*100:>5.1f}%')
    if b == -1: best = net

# --- 籠として: 1 事象 1 枠、資金の 1/3 を上限に、日次の資産曲線 ---
print('\n=== 籠（1 事象 = 資金の 1/3、−4 建て −1 決め、TOPIX ヘッジ、費用込み）===')
eq = pd.Series(0.0, index=tdays)
for _, row in up.iterrows():
    a, b = row.ti - 4, row.ti - 1
    if a < 0 or b >= T: continue
    r = (OP[b, row.ci] / OP[a, row.ci] - 1) - (tpo[b] / tpo[a] - 1) - COST / 1e4
    if np.isnan(r): continue
    eq.iloc[b] += r / 3.0
d = stats(eq)
print(f'  全期間  {fmt(d)}')
i, o = split(eq)
print(f'  IS      {fmt(stats(i))}')
print(f'  OOS     {fmt(stats(o))}')
print(f'  1 事象の売買代金 20 日平均の中央値  {up.adv.median()/1e8:.1f} 億円')
print(f'  1 事象の純 bp の中央値  {best.median():.1f}（平均 {best.mean():.1f}）')

# --- 決定的な確認: 跳ねの日は本当に常に −5 か（発表の遅れが一定か）---
print('\n=== 事象ごとに |超過リターン| が最大だった日（−8〜0）の分布 ===')
days, ret = [], []
for _, row in up.iterrows():
    v = []
    for k in range(-8, 1):
        a, b = row.ti + k, row.ti + k + 1
        if a < 0 or b >= T: v.append(np.nan); continue
        v.append((OP[b, row.ci] / OP[a, row.ci] - 1) - (tpo[b] / tpo[a] - 1))
    v = np.array(v)
    if np.isnan(v).all(): continue
    days.append(range(-8, 1)[int(np.nanargmax(np.abs(v)))])
    ret.append(np.nanmax(np.abs(v)))
dist = pd.Series(days).value_counts().sort_index()
for k, n in dist.items(): print(f'  {k:>+3} 日  {n:>4} 件  {n/len(days)*100:5.1f}%')
