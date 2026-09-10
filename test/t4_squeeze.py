# T4. ボラティリティ・スクイーズの放れ。バンド幅が過去 120 日で最低水準の銘柄が 20 日高値（安値）を抜けた後、
# 方向が続くか。時点母集団の大型 500、翌寄りから 5 / 10 / 20 日、TOPIX 超過、IS/OOS 別。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, AdjC, H, L, C AS Craw, MktCap FROM bars WHERE AdjC>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
r = px.AdjC / px.Craw; px['AdjH'] = px.H * r; px['AdjL'] = px.L * r
def wide(col): return px.pivot(index='Date', columns='Code', values=col).reindex(tdays)
C, O, H, L, M = wide('AdjC'), wide('AdjO'), wide('AdjH'), wide('AdjL'), wide('MktCap')
del px
codes = list(C.columns)
master = c.execute("SELECT Date, Code FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
master['Date'] = pd.to_datetime(master.Date); mset = master.groupby('Date').Code.apply(set)
mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
elig = pd.DataFrame(False, index=tdays, columns=codes)
for k in range(len(mends) - 1):
    me, nx = mends.iloc[k], mends.iloc[k + 1]
    prior = mset.index[mset.index <= me]
    if not len(prior): continue
    cap = M.loc[me]; cap = cap[cap.index.isin(mset[prior[-1]])].dropna().sort_values(ascending=False).head(500)
    elig.loc[(elig.index > me) & (elig.index <= nx), cap.index] = True
ELIG = elig.values; print('eligible stock-days', ELIG.sum(), flush=True)

sma = C.rolling(20).mean(); sd = C.rolling(20).std()
bbw = (2 * sd / sma)                                  # ボリンジャーのバンド幅
pct = bbw.rolling(120).rank(pct=True).values          # 過去 120 日の中での位置（0 = 最も狭い）
CL, OP = C.values, O.values
hh = H.shift(1).rolling(20).max().values; ll = L.shift(1).rolling(20).min().values
up = (CL > hh) & ELIG; dn = (CL < ll) & ELIG
tpc, tpo = tp.close.values, tp.open.values
IS = np.asarray(tdays <= IS_END)

def stat(mask, h, side):
    ent = np.roll(OP, -1, axis=0); ex = np.roll(CL, -1 - h, axis=0)
    exc = (ex / ent - 1) - (np.roll(tpc, -1 - h) / np.roll(tpo, -1) - 1)[:, None]
    if side < 0: exc = -exc                            # 下放れは売り側の超過
    v = ~np.isnan(exc); v[-(h + 2):, :] = False
    out = []
    for lab, per in [('IS', IS), ('OOS', ~IS)]:
        m = mask & v & per[:, None]; x = exc[m]
        out.append((lab, len(x), x.mean() * 1e4, x.mean() / x.std() * np.sqrt(len(x)) if len(x) > 1 else np.nan))
    return out

print(f"\n=== スクイーズ（バンド幅が過去 120 日の下位 X%）からの放れ → 翌寄りから h 日の TOPIX 超過")
print(f"{'条件':<34}{'h':>3}  {'IS 件数':>8}{'IS bp':>8}{'t':>6}   {'OOS 件数':>8}{'OOS bp':>8}{'t':>6}")
for lab, base, side in [('上放れ（20 日高値抜け）', up, +1), ('下放れ（20 日安値割れ）', dn, -1)]:
    for xlab, sq in [('スクイーズ条件なし', np.ones_like(pct, bool)), ('下位 20%', pct <= 0.2), ('下位 10%', pct <= 0.1)]:
        m = base & sq & ~np.isnan(pct)
        for h in [5, 10, 20]:
            (a, an, ab, at), (b, bn, bb_, bt) = stat(m, h, side)
            print(f"{lab} {xlab:<16}{h:>3}  {an:>8,}{ab:>8.1f}{at:>6.1f}   {bn:>8,}{bb_:>8.1f}{bt:>6.1f}")
print("\n（超過は TOPIX 比。上放れは買い、下放れは売りの側で符号を揃えた。プラスなら順張りが効く）")
