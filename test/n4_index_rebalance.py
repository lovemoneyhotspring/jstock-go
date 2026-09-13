# N4. 指数入替のリバランス需給。master の日次スナップショットから ScaleCat（TOPIX 規模区分）と
# Mkt（市場区分）の変更を検出し、実施日 t の前後を測る。公表日は取れないので「事前の買い上げ」と
# 「実施後の反動」を分けて見る。母集団は流動性で切り、当日の母集団平均で中立化する。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC, TRY_CAST(Va AS DOUBLE) Va FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
V = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)
OP = O.values

# --- 母集団: 20 日平均売買代金 1 億円以上（規模区分の入替は中小型で起きるので上位 500 では狭すぎる）---
ADV = V.rolling(20).mean().reindex(columns=codes).values
ELIG = ADV >= 1e8
print('母集団 銘柄日', np.nansum(ELIG))

# --- 事象: master の ScaleCat / Mkt が変わった最初の日 ---
m = c.execute("SELECT Date, Code, ScaleCat, Mkt FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
m['Date'] = pd.to_datetime(m.Date)
m = m.sort_values(['Code', 'Date'])
m['pScale'] = m.groupby('Code').ScaleCat.shift(1)
m['pMkt'] = m.groupby('Code').Mkt.shift(1)

RANK = {'TOPIX Core30': 5, 'TOPIX Large70': 4, 'TOPIX Mid400': 3, 'TOPIX Small 1': 2, 'TOPIX Small 2': 1, '-': 0}
PRIME = {'0101', '0111'}    # 東証一部 / プライム（2022-04 までは TOPIX 採用の条件）

sc = m[(m.ScaleCat != m.pScale) & m.pScale.notna()].copy()
sc['up'] = sc.ScaleCat.map(RANK).fillna(0) > sc.pScale.map(RANK).fillna(0)
mk = m[(m.Mkt != m.pMkt) & m.pMkt.notna()].copy()
mk['up'] = mk.Mkt.isin(PRIME) & ~mk.pMkt.isin(PRIME)
mk['down'] = ~mk.Mkt.isin(PRIME) & mk.pMkt.isin(PRIME)

tpos = {d: i for i, d in enumerate(tdays)}
def prep(df):
    df = df.copy()
    df['ti'] = df.Date.map(tpos); df['ci'] = df.Code.map(cpos)
    df = df.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
    ok = ELIG[df.ti.values, df.ci.values]
    return df[np.where(np.isnan(ok), False, ok).astype(bool)]
sc, mk = prep(sc), prep(mk)
print('規模区分の変更', len(sc), ' 市場区分の変更', len(mk))
print(sc.groupby(sc.Date.dt.to_period('Y')).size().to_string())

def window(ti, ci, a, b):
    """t+a 寄り → t+b 寄りの、母集団当日平均で中立化したリターン"""
    ia, ib = ti + a, ti + b
    ok = (ia >= 0) & (ib < T) & (ia < T) & (ib >= 0)
    r = np.full(len(ti), np.nan)
    r[ok] = OP[ib[ok], ci[ok]] / OP[ia[ok], ci[ok]] - 1
    base = {}
    for t in np.unique(ti[ok]):
        if t + a < 0 or t + b >= T: continue
        mm = ELIG[t] & ~np.isnan(OP[t + a]) & ~np.isnan(OP[t + b])
        if mm.sum() < 50: continue
        base[t] = (OP[t + b, mm] / OP[t + a, mm] - 1).mean()
    r[ok] -= np.array([base.get(t, np.nan) for t in ti[ok]])
    return r

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan

def show(name, df):
    print(f'\n=== {name}（母集団中立 bp、事象でクラスタせず単純平均。t は事象単位）===')
    print(f'{"窓":<22} {"件数":>6} {"全期間":>9} {"t":>6} {"IS":>9} {"t":>6} {"OOS":>9} {"t":>6}')
    for label, a, b in [('前 −20 → 実施日 0', -20, 0), ('前 −5 → 実施日 0', -5, 0),
                        ('実施 0 → +5', 0, 5), ('実施 0 → +10', 0, 10),
                        ('実施 0 → +20', 0, 20), ('実施 0 → +60', 0, 60)]:
        y = window(df.ti.values, df.ci.values, a, b)
        s = pd.Series(y, index=df.Date.values).dropna()
        if len(s) < 20: continue
        i, o = s[s.index <= IS_END], s[s.index > IS_END]
        f = lambda x: (x.mean()*1e4, tv(x)) if len(x) > 2 else (np.nan, np.nan)
        (am, at), (im, it), (om, ot) = f(s), f(i), f(o)
        print(f'{label:<22} {len(s):>6} {am:>9.1f} {at:>6.2f} {im:>9.1f} {it:>6.2f} {om:>9.1f} {ot:>6.2f}')

show('TOPIX 規模区分 昇格（Small2→Small1→Mid400→Large70→Core30）', sc[sc.up])
show('TOPIX 規模区分 降格', sc[~sc.up])
show('市場区分 昇格（→ 東証一部 / プライム）', mk[mk.up])
show('市場区分 降格（東証一部 / プライム →）', mk[mk.down])

# --- 追加: 市場区分の昇格・降格の件数分布と、細かい窓 ---
print('\n市場区分 昇格の年別件数'); print(mk[mk.up].groupby(mk[mk.up].Date.dt.to_period('Y')).size().to_string())
print('\n市場区分 降格の年別件数'); print(mk[mk.down].groupby(mk[mk.down].Date.dt.to_period('Y')).size().to_string())

def fine(name, df, sign):
    print(f'\n=== {name}: 細かい窓（母集団中立 bp）===')
    print(f'{"窓":<18} {"件数":>6} {"全期間":>9} {"t":>6} {"IS":>9} {"t":>6} {"OOS":>9} {"t":>6}')
    for a, b in [(-20, -10), (-10, -5), (-5, -3), (-3, -1), (-1, 0), (0, 1), (0, 3)]:
        y = window(df.ti.values, df.ci.values, a, b) * sign
        s = pd.Series(y, index=df.Date.values).dropna()
        if len(s) < 20: continue
        i, o = s[s.index <= IS_END], s[s.index > IS_END]
        f = lambda x: (x.mean()*1e4, tv(x)) if len(x) > 2 else (np.nan, np.nan)
        (am, at), (im, it), (om, ot) = f(s), f(i), f(o)
        print(f'{f"{a:+d} → {b:+d}":<18} {len(s):>6} {am:>9.1f} {at:>6.2f} {im:>9.1f} {it:>6.2f} {om:>9.1f} {ot:>6.2f}')

fine('市場区分 昇格（買い）', mk[mk.up], +1)
fine('市場区分 降格（売り、符号反転済み）', mk[mk.down], -1)

# --- 追加: 昇格の 1 日刻みプロファイル（跳ねが 1 日に集中していないか）---
print('\n=== 市場区分 昇格: 1 日刻み（母集団中立 bp、寄り → 寄り）===')
print(f'{"日":>5} {"全期間":>9} {"t":>6} {"IS":>9} {"OOS":>9}')
d = mk[mk.up]
for k in range(-8, 4):
    y = window(d.ti.values, d.ci.values, k, k + 1)
    s = pd.Series(y, index=d.Date.values).dropna()
    if len(s) < 20: continue
    i, o = s[s.index <= IS_END], s[s.index > IS_END]
    print(f'{k:>+5} {s.mean()*1e4:>9.1f} {tv(s):>6.2f} {i.mean()*1e4:>9.1f} {o.mean()*1e4:>9.1f}')
