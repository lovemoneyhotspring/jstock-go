# N5. 信用の規制イベント（増担保・日々公表・貸借規制）の発動と解除。価格に無関心な建玉整理が起きる需給事象。
# markets_margin_alert は「その日に規制がかかっている銘柄」だけが並ぶので、発動＝在の開始、解除＝在の終了で取る。
# 公表は 16:30 なので翌寄り建て。母集団の当日平均で中立化。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
c.execute(f"CREATE VIEW ma AS SELECT * FROM read_parquet('{ROOT}/markets_margin_alert/*.parquet', union_by_name=true)")
tp = topix(c); tdays = tp.index
px = c.execute("SELECT Date, Code, AdjO, TRY_CAST(Va AS DOUBLE) Va FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
V = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays)
del px
codes = list(O.columns); cpos = {k: i for i, k in enumerate(codes)}; T = len(tdays)
OP = O.values; tpo = tp.open.values
ELIG = (V.rolling(20).mean().values >= 1e8)

FLAGS = ['Restricted', 'DailyPublication', 'RestrictedByJSF', 'PrecautionByJSF']
JA = {'Restricted': '増担保（信用規制）', 'DailyPublication': '日々公表',
      'RestrictedByJSF': '貸借取引の規制', 'PrecautionByJSF': '貸借の注意喚起'}
raw = c.execute("SELECT PubDate, Code, PubReason FROM ma").df()
raw['PubDate'] = pd.to_datetime(raw.PubDate)
for f in FLAGS:
    raw[f] = raw.PubReason.str.extract(f'["\']{f}["\']: ["\'](\\d)', expand=False).astype(float)
raw = raw.drop(columns='PubReason')
print('公表行', len(raw), ' 欠損', int(raw[FLAGS].isna().all(axis=1).sum()))

pubdays = np.array(sorted(raw.PubDate.unique()))
pidx = {d: i for i, d in enumerate(pubdays)}
tpos = {d: i for i, d in enumerate(tdays)}

def transitions(f):
    """flag=1 の (Code, 公表日) から、在の開始（発動）と終了の翌公表日（解除）を作る"""
    d = raw.loc[raw[f] == 1, ['Code', 'PubDate']].drop_duplicates().sort_values(['Code', 'PubDate'])
    d['p'] = d.PubDate.map(pidx)
    d['prev'] = d.groupby('Code').p.shift(1)
    d['next'] = d.groupby('Code').p.shift(-1)
    on = d[d.prev.isna() | (d.p - d.prev > 1)][['Code', 'PubDate']]
    last = d[d['next'].isna() | (d['next'] - d.p > 1)].copy()
    last['off_p'] = (last.p + 1).clip(upper=len(pubdays) - 1)
    off = pd.DataFrame({'Code': last.Code.values, 'PubDate': pubdays[last.off_p.values.astype(int)]})
    return on, off

def prep(e):
    e = e.copy()
    e['ti'] = e.PubDate.map(tpos); e['ci'] = e.Code.map(cpos)
    e = e.dropna(subset=['ti', 'ci']).astype({'ti': int, 'ci': int})
    ok = ELIG[e.ti.values, e.ci.values]
    return e[np.where(np.isnan(ok), False, ok).astype(bool)]

def window(ti, ci, a, b):
    ia, ib = ti + a, ti + b
    ok = (ia >= 0) & (ib < T)
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

COST = 31.4
print(f'\n{"事象":<22} {"件数":>6} {"窓":>5} {"純bp":>8} {"t":>6} {"IS純":>8} {"IS t":>6} {"OOS純":>8} {"OOS t":>6} {"勝率":>6} {"中央値":>8}')
keep = []
for f in FLAGS:
    on, off = transitions(f)
    for kind, e in [('発動', prep(on)), ('解除', prep(off))]:
        if len(e) < 100: continue
        for b in (1, 3, 5, 10, 20):
            y = window(e.ti.values, e.ci.values, 1, 1 + b)
            s = pd.Series(y, index=e.PubDate.values).dropna()
            if len(s) < 100: continue
            net = s * 1e4 - COST
            i, o = net[net.index <= IS_END], net[net.index > IS_END]
            if len(i) < 30 or len(o) < 30: continue
            print(f'{JA[f]+" "+kind:<22} {len(s):>6} {b:>4}日 {net.mean():>8.1f} {tv(net):>6.2f} '
                  f'{i.mean():>8.1f} {tv(i):>6.2f} {o.mean():>8.1f} {tv(o):>6.2f} '
                  f'{(net>0).mean()*100:>5.1f}% {net.median():>8.1f}')
            if np.sign(i.mean()) == np.sign(o.mean()) and abs(tv(i)) >= 2 and abs(tv(o)) >= 2:
                keep.append((JA[f], kind, b, len(s), net.mean(), (net > 0).mean()*100, net.median()))
print('\n両半期で同符号かつ |t|≥2:')
for k in keep: print('  ', k)
