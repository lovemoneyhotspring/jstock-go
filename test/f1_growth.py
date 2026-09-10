# F1. 財務諸表から 3 年を予測して買い持ちできるか。(1) 成長率は持続するか (2) 成長・質・割安さの十分位は
# 3 年保有リターン（TOPIX 超過）を分けるか。時点母集団、開示日の翌営業日で建てる。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
f = c.execute("""SELECT DiscDate, DiscTime, Code, CurFYEn, TRY_CAST(Sales AS DOUBLE) Sales,
  TRY_CAST(OP AS DOUBLE) OP, TRY_CAST(NP AS DOUBLE) NP, TRY_CAST(Eq AS DOUBLE) Eq, TRY_CAST(TA AS DOUBLE) TA
  FROM fins WHERE CurPerType='FY' AND DocType LIKE 'FYFinancialStatements%' AND Sales IS NOT NULL""").df()
f['DiscDate'] = pd.to_datetime(f.DiscDate); f['fy'] = pd.to_datetime(f.CurFYEn).dt.year
f = f.sort_values('DiscDate').drop_duplicates(['Code', 'fy'], keep='last')
print(f"FY 決算 {len(f):,} 件  {f.fy.min()}〜{f.fy.max()}")

px = c.execute("SELECT Date, Code, AdjC, MktCap FROM bars WHERE AdjC>0").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
MC = px.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays); del px
pos = pd.Series(range(len(tdays)), index=tdays)
# 建てる日 = 開示日の翌営業日（15:00 以降の開示でも翌日なので安全側）
ei_ = np.searchsorted(tdays.values, f.DiscDate.values, side='right')
f = f[ei_ < len(tdays)].copy(); f['ei'] = ei_[ei_ < len(tdays)]
f = f[f.ei + 750 < len(tdays)]                 # 3 年（750 営業日）先まで価格がある

# --- 3 年保有の TOPIX 超過 ---
cv = C.values; tpv = tp.close.values; cols = {k: i for i, k in enumerate(C.columns)}
f = f[f.Code.isin(cols)]
ci = f.Code.map(cols).values; ei = f.ei.values
p0 = cv[ei, ci]; p1 = cv[ei + 750, ci]
f['ret3'] = p1 / p0 - 1
f['exc3'] = f.ret3 - (tpv[ei + 750] / tpv[ei] - 1)
f['mcap'] = MC.values[ei, ci]
# --- 指標 ---
g = f.set_index(['Code', 'fy'])
for col in ['Sales', 'OP', 'NP']:
    base = g[col].reindex(pd.MultiIndex.from_arrays([f.Code, f.fy - 3])).values
    f[f'g3_{col}'] = np.where(base > 0, f[col].values / base - 1, np.nan)
    nxt = g[col].reindex(pd.MultiIndex.from_arrays([f.Code, f.fy + 3])).values
    f[f'fwd3_{col}'] = np.where(f[col].values > 0, nxt / f[col].values - 1, np.nan)
f['mcap'] = f.mcap * 1e6                              # MktCap は百万円
f['roe'] = f.NP / f.Eq; f['ep'] = f.NP / f.mcap; f['bp'] = f.Eq / f.mcap; f['eqr'] = f.Eq / f.TA
u = f[(f.mcap > 1e10) & f.exc3.notna() & f.Eq.notna()].copy()      # 時価総額 100 億円以上
u['big'] = u.groupby('fy').mcap.rank(ascending=False) <= 500
print(f"検定に使える銘柄年 {len(u):,}（うち大型 500 {u.big.sum():,}）  コホート {sorted(u.fy.unique())}")

print("\n=== (1) 成長率は持続するか（過去 3 年の成長 → 次の 3 年の成長、順位相関）")
for col in ['Sales', 'OP']:
    print(f"  {col}:")
    for fy, x in u[u.big].groupby('fy'):
        x = x[[f'g3_{col}', f'fwd3_{col}']].dropna()
        if len(x) < 50: continue
        r = x.iloc[:, 0].rank().corr(x.iloc[:, 1].rank())
        t = r * np.sqrt((len(x) - 2) / max(1e-9, 1 - r ** 2))
        print(f"    {fy} 期  n {len(x):4d}  ρ {r:+.3f}  t {t:+5.1f}")

print("\n=== (2) 十分位 → 3 年保有の TOPIX 超過（大型 500、開示翌営業日から 750 営業日）")
def dec(col, lab):
    x = u[u.big & u[col].notna()].copy()
    if len(x) < 500: return
    x['d'] = x.groupby('fy')[col].transform(lambda s: pd.qcut(s, 5, labels=False, duplicates='drop'))
    a = x.groupby('d').exc3.agg(['size', 'mean'])
    hi, lo = x[x.d == a.index.max()].exc3, x[x.d == 0].exc3
    sp = hi.mean() - lo.mean()
    t = sp / np.sqrt(hi.var() / len(hi) + lo.var() / len(lo))
    print(f"  {lab:<22} " + "  ".join(f"D{int(k)+1} {v['mean']*100:+6.1f}%" for k, v in a.iterrows()) + f"   D5−D1 {sp*100:+6.1f}pt (t {t:4.1f})")
for col, lab in [('g3_Sales', '過去3年の売上成長'), ('g3_OP', '過去3年の営業利益成長'), ('roe', 'ROE'),
                 ('ep', '益回り NP/時価総額'), ('bp', '純資産/時価総額'), ('eqr', '自己資本比率')]:
    dec(col, lab)
print("\n=== 参考: コホート別に「上位20%を3年持つ」の TOPIX 超過（大型 500）")
for col, lab in [('g3_OP', '営業利益成長 上位20%'), ('roe', 'ROE 上位20%'), ('ep', '益回り 上位20%')]:
    x = u[u.big & u[col].notna()].copy()
    x['d'] = x.groupby('fy')[col].transform(lambda s: pd.qcut(s, 5, labels=False, duplicates='drop'))
    top = x[x.d == 4]
    print(f"  {lab:<18} " + "  ".join(f"{int(fy)}期 {gg.exc3.mean()*100:+5.1f}%" for fy, gg in top.groupby('fy')))

print("\n=== IS（2015〜2019 期）/ OOS（2020〜2023 期）に割ると D5−D1")
for col, lab in [('g3_Sales','過去3年の売上成長'), ('g3_OP','過去3年の営業利益成長'), ('roe','ROE'),
                 ('ep','益回り'), ('bp','純資産/時価総額')]:
    x = u[u.big & u[col].notna()].copy()
    x['d'] = x.groupby('fy')[col].transform(lambda s: pd.qcut(s, 5, labels=False, duplicates='drop'))
    out=[]
    for lab2, m in [('IS', x.fy <= 2019), ('OOS', x.fy >= 2020)]:
        y = x[m]; hi, lo = y[y.d==4].exc3, y[y.d==0].exc3
        if len(hi)<30 or len(lo)<30: out.append(f"{lab2} n/a"); continue
        sp = hi.mean()-lo.mean(); t = sp/np.sqrt(hi.var()/len(hi)+lo.var()/len(lo))
        out.append(f"{lab2} {sp*100:+6.1f}pt (t {t:+4.1f}, n {len(hi)+len(lo)})")
    print(f"  {lab:<22} " + "   ".join(out))
