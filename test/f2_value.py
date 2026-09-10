# F2. 割安さ（益回り・PBR）は使えるか。(A) 月次リバランスの横断検定と籠 (B) daytrade の候補フィルタ。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

COST = 0.001   # 片道
c = con(); tp = topix(c); tdays = tp.index
f = c.execute("""SELECT DiscDate, Code, TRY_CAST(NP AS DOUBLE) NP, TRY_CAST(Eq AS DOUBLE) Eq
  FROM fins WHERE CurPerType='FY' AND DocType LIKE 'FYFinancialStatements%' AND NP IS NOT NULL""").df()
f['DiscDate'] = pd.to_datetime(f.DiscDate); f = f.sort_values('DiscDate')
px = c.execute("SELECT Date, Code, AdjC, MktCap FROM bars WHERE AdjC>0").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
MC = px.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays) * 1e6   # 百万円 → 円
del px
# 開示済みの最新の本決算を日次に展開（開示日の翌営業日から有効）
NP = pd.DataFrame(np.nan, index=tdays, columns=C.columns); EQ = NP.copy()
ei = np.searchsorted(tdays.values, f.DiscDate.values, side='right')
ok = (ei < len(tdays)) & f.Code.isin(C.columns)
ci = {k: n for n, k in enumerate(C.columns)}
for name, src in [('NP', f.NP.values), ('EQ', f.Eq.values)]:
    tmp = pd.DataFrame({'i': ei[ok], 'code': f.Code.values[ok], 'v': src[ok]}).drop_duplicates(['i','code'], keep='last')
    m = np.full((len(tdays), len(C.columns)), np.nan)
    m[tmp.i.values, [ci[k] for k in tmp.code]] = tmp.v.values
    if name == 'NP': NP = pd.DataFrame(m, index=tdays, columns=C.columns)
    else: EQ = pd.DataFrame(m, index=tdays, columns=C.columns)
NP = NP.ffill(); EQ = EQ.ffill()
EP = NP / MC; BP = EQ / MC
master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date); mset = master.groupby('Date').Code.apply(set)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
mends = mends[mends >= '2016-12-01']

def monthly(sig, lab):
    rows = []
    for k in range(len(mends) - 1):
        d0, d1 = mends.iloc[k], mends.iloc[k + 1]
        prior = mset.index[mset.index <= d0]
        if not len(prior): continue
        cap = MC.loc[d0]; cap = cap[cap.index.isin(mset[prior[-1]])].dropna().sort_values(ascending=False).head(500)
        s = sig.loc[d0, cap.index].dropna()
        r = (C.loc[d1, s.index] / C.loc[d0, s.index] - 1) - (tp.close[d1] / tp.close[d0] - 1)
        x = pd.DataFrame({'s': s, 'r': r}).dropna()
        if len(x) < 100: continue
        x['q'] = pd.qcut(x.s, 5, labels=False, duplicates='drop')
        g = x.groupby('q').r.mean()
        rows.append(pd.Series(g, name=d1))
    m = pd.DataFrame(rows)
    print(f"\n--- {lab}（月次、TOPIX 超過 bp、{len(m)} か月）")
    for per, mm in [('全体', m), ('IS 〜2022', m[m.index <= IS_END]), ('OOS 2023〜', m[m.index > IS_END])]:
        sp = mm[4] - mm[0]
        print(f"  {per:<10} " + "  ".join(f"D{i+1} {mm[i].mean()*1e4:+6.1f}" for i in range(5)) +
              f"   D5−D1 {sp.mean()*1e4:+7.1f} bp/月 (t {sp.mean()/sp.std()*np.sqrt(len(sp)):+4.1f}, 年 {sp.mean()*12*100:+5.1f}%)")
    return m

for sig, lab in [(EP, '益回り NP/時価総額'), (BP, '純資産/時価総額（PBR の逆数）')]:
    monthly(sig, lab)

print("\n=== (A2) 籠: 最割安 20%（100 銘柄）を等金額・月次入替、絶対リターン")
def basket(sig, lab, top=True):
    eq = [1.0]; dates = [mends.iloc[0]]; prev = set()
    for k in range(len(mends) - 1):
        d0, d1 = mends.iloc[k], mends.iloc[k + 1]
        prior = mset.index[mset.index <= d0]
        if not len(prior): continue
        cap = MC.loc[d0]; cap = cap[cap.index.isin(mset[prior[-1]])].dropna().sort_values(ascending=False).head(500)
        s = sig.loc[d0, cap.index].dropna()
        if len(s) < 100: continue
        pick = set(s.nlargest(100).index if top else s.nsmallest(100).index)
        r = (C.loc[d1, list(pick)] / C.loc[d0, list(pick)] - 1).dropna().mean()
        turn = len(pick - prev) / max(1, len(pick))
        eq.append(eq[-1] * (1 + r - turn * COST * 2)); dates.append(d1); prev = pick
    e = pd.Series(eq, index=dates)
    r = e.pct_change().dropna()
    for per, rr in [('全体', r), ('IS', r[r.index <= IS_END]), ('OOS', r[r.index > IS_END])]:
        d = stats(rr, n=12); print(f"  {lab:<16} {per:<5} {fmt(d)}")
for sig, lab in [(EP, '益回り 上位100'), (BP, 'PBR 低い100')]:
    basket(sig, lab)
tpm = tp.close.reindex(mends).pct_change().dropna()
for per, rr in [('全体', tpm), ('IS', tpm[tpm.index <= IS_END]), ('OOS', tpm[tpm.index > IS_END])]:
    print(f"  {'TOPIX 買い持ち':<16} {per:<5} {fmt(stats(rr, n=12))}")

print("\n=== (B) daytrade の候補に割安さは効くか（trades_10y、建てた日の益回り 3 分位）")
t = pd.read_csv(f'{OUT}/trades_10y.csv'); t['date'] = pd.to_datetime(t.date); t['code'] = t.code.astype(str)
t['bp'] = t.pnl / t.amount * 1e4
epl = EP.stack().rename('ep').reset_index(); epl.columns = ['date', 'code', 'ep']
t = t.merge(epl, on=['date', 'code'], how='left')
print(f"  突合できた取引 {t.ep.notna().sum():,}/{len(t):,}")
for side, g in t.dropna(subset=['ep']).groupby('side'):
    g = g.copy(); g['q'] = pd.qcut(g.ep, 3, labels=['割高', '中位', '割安'])
    a = g.groupby('q', observed=True).agg(n=('bp','size'), bp=('bp','mean'), pnl=('pnl','sum'))
    a['t'] = g.groupby('q', observed=True).bp.apply(lambda x: x.mean()/x.std()*np.sqrt(len(x)))
    print(f"  {side}: " + "  ".join(f"{k} {r.bp:+.1f}bp (t {r.t:.1f}, n {int(r.n)})" for k, r in a.iterrows()))

print("\n=== (B2) 頑健性: IS/OOS 分割、赤字企業の扱い、PBR でも同じか")
t['IS'] = t.date <= IS_END
bpl = BP.stack().rename('bp_'); bpl = bpl.reset_index(); bpl.columns = ['date','code','bpr']
t = t.merge(bpl, on=['date','code'], how='left')
L = t[(t.side=='long') & t.ep.notna()].copy()
def tri(x, col): return pd.qcut(x[col], 3, labels=['割高','中位','割安'])
for lab, sub in [('IS 〜2022', L[L.IS]), ('OOS 2023〜', L[~L.IS])]:
    s = sub.copy(); s['q'] = tri(s, 'ep')
    a = s.groupby('q', observed=True).bp.agg(['size','mean'])
    a['t'] = s.groupby('q', observed=True).bp.apply(lambda x: x.mean()/x.std()*np.sqrt(len(x)))
    print(f"  益回り {lab:<10} " + "  ".join(f"{k} {r['mean']:+6.1f}bp (t {r.t:4.1f}, n {int(r['size'])})" for k,r in a.iterrows()))
pos = L[L.ep > 0].copy(); pos['q'] = tri(pos, 'ep')
a = pos.groupby('q', observed=True).bp.agg(['size','mean'])
a['t'] = pos.groupby('q', observed=True).bp.apply(lambda x: x.mean()/x.std()*np.sqrt(len(x)))
print(f"  黒字だけ（NP>0） " + "  ".join(f"{k} {r['mean']:+6.1f}bp (t {r.t:4.1f}, n {int(r['size'])})" for k,r in a.iterrows()))
print(f"  赤字（NP≤0）の取引 {len(L)-len(pos)} 件  平均 {L[L.ep<=0].bp.mean():+.1f}bp")
B = L[L.bpr.notna()].copy(); B['q'] = tri(B, 'bpr')
a = B.groupby('q', observed=True).bp.agg(['size','mean'])
a['t'] = B.groupby('q', observed=True).bp.apply(lambda x: x.mean()/x.std()*np.sqrt(len(x)))
print(f"  PBR（純資産/時価）  " + "  ".join(f"{k} {r['mean']:+6.1f}bp (t {r.t:4.1f}, n {int(r['size'])})" for k,r in a.iterrows()))
# ギャップの深さと割安さは混ざっていないか
L['gq'] = pd.qcut(L.gap, 3, labels=['浅','中','深']); L['vq'] = tri(L,'ep')
print("\n  ギャップの深さ × 割安さ（平均 bp / 件数）")
piv = L.pivot_table(index='gq', columns='vq', values='bp', aggfunc='mean', observed=True)
cnt = L.pivot_table(index='gq', columns='vq', values='bp', aggfunc='size', observed=True)
for i in piv.index: print("   ", i, "  ".join(f"{c} {piv.loc[i,c]:+6.1f}bp({int(cnt.loc[i,c])})" for c in piv.columns))

print("\n=== (B3) 割安 − 割高 の差の検定（ロング）")
for lab, sub in [('全体', L), ('IS 〜2022', L[L.IS]), ('OOS 2023〜', L[~L.IS]), ('黒字だけ', L[L.ep>0])]:
    s = sub.copy(); s['q'] = pd.qcut(s.ep, 3, labels=['割高','中位','割安'])
    a = s[s.q=='割安'].bp; b = s[s.q=='割高'].bp
    d = a.mean()-b.mean(); tt = d/np.sqrt(a.var()/len(a)+b.var()/len(b))
    print(f"  {lab:<10} 割安 {a.mean():+6.1f} − 割高 {b.mean():+6.1f} = {d:+6.1f} bp  (t {tt:+4.1f})")
print("\n=== (B4) 赤字銘柄を外すだけの効果（ロング、現行の取引をそのまま）")
allL = t[(t.side=='long')]
neg = allL[allL.ep<=0]
print(f"  赤字（直近本決算 NP≤0）{len(neg)} 件  損益 {neg.pnl.sum():,.0f} 円  平均 {neg.bp.mean():+.1f} bp")
print(f"  除外後 {len(allL)-len(neg)} 件  損益 {allL.pnl.sum()-neg.pnl.sum():,.0f} 円（全体 {allL.pnl.sum():,.0f} 円）")
