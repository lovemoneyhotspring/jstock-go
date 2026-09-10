# T3. 悪い決算の売り。PEAD の事象表（a_events.parquet）の EAR 最下位十分位を、反応翌日の終値が反応日の安値を割った（確認）翌寄りで売り、
# 10 / 20 / 40 営業日の引けで買い戻す。貸借銘柄のみ。片道 0.10% + 貸株料 1.1%/年の日割り。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import itertools

COST = 0.001; LEND = 0.011; SLOTS = 30
c = con()
tp = topix(c); tdays = tp.index; tidx = pd.Series(np.arange(len(tdays)), index=tdays)
u = pd.read_parquet(f'{OUT}/a_events.parquet')
u['ym'] = u.rdate.dt.to_period('M')
u['dec'] = u.groupby('ym').ear.transform(lambda s: pd.qcut(s.rank(method='first'), 10, labels=False) + 1 if len(s) >= 20 else np.nan)
d1 = u[u.dec == 1].copy()
# 貸借銘柄（反応日）
mg = c.execute("SELECT Date, Code FROM master WHERE Mrgn='2'").df(); mg['Date'] = pd.to_datetime(mg.Date)
mgset = set(zip(mg.Date, mg.Code))
d1 = d1[[(d, cd) in mgset for d, cd in zip(d1.rdate, d1.Code)]].copy()
print('D1 事象', (u.dec == 1).sum(), '→ 貸借', len(d1), d1.groupby('period').size().to_dict())

# 価格
codes = tuple(d1.Code.unique())
px = c.execute(f"SELECT Code, Date, AdjO, AdjC, L*AdjC/C AS AdjL FROM bars WHERE Code IN {codes} AND AdjC>0 AND C>0").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]; px['pos'] = tidx.reindex(px.Date).values
key = px.set_index(['Code', 'pos']); adjc = key.AdjC; adjo = key.AdjO; adjl = key.AdjL
def get(s, code, p): return s.reindex(list(zip(code, p))).values
code = d1.Code.values; rp = d1.rpos.values
d1['low_r'] = get(adjl, code, rp); d1['c_r1'] = get(adjc, code, rp + 1)
d1['confirm'] = d1.c_r1 < d1.low_r
tpc = tp.close.values; tpo = tp.open.values

def entry_pos(conf): return rp + (2 if conf else 1)   # 確認あり: 反応日+2 の寄り、なし: 反応日+1 の寄り

rows = []
print("\n== 横断: 売りの平均リターン（bp、正が利益）、括弧は t 値。絶対 / TOPIX 中立")
for conf, h in itertools.product([True, False], [10, 20, 40]):
    ep = entry_pos(conf)
    sel = d1.confirm.values if conf else np.ones(len(d1), bool)
    e = get(adjo, code, ep); x = get(adjc, code, ep + h - 1)
    r_abs = -(x / e - 1) - 2 * COST - LEND * h / 245
    r_mkt = (tpc[np.minimum(ep + h - 1, len(tdays) - 1)] / tpo[np.minimum(ep, len(tdays) - 1)] - 1)
    r_neu = r_abs + r_mkt
    ok = sel & ~np.isnan(r_abs) & (ep + h - 1 < len(tdays))
    for per in ['IS', 'OOS']:
        m = ok & (d1.period.values == per)
        a, n_ = r_abs[m], r_neu[m]
        rows.append(dict(kind='xsec', confirm=conf, hold=h, period=per, n=m.sum(), abs_bp=a.mean() * 1e4, abs_t=a.mean() / a.std() * np.sqrt(len(a)), neu_bp=n_.mean() * 1e4, neu_t=n_.mean() / n_.std() * np.sqrt(len(n_)), win=(a > 0).mean()))
    r = [x for x in rows if x['kind'] == 'xsec' and x['confirm'] == conf and x['hold'] == h]
    print(f"  確認{'あり' if conf else 'なし'} {h:2d}日  IS n={r[0]['n']:5d} {r[0]['abs_bp']:+5.0f} (t{r[0]['abs_t']:+.1f}) / {r[0]['neu_bp']:+5.0f} (t{r[0]['neu_t']:+.1f}) 勝率 {r[0]['win']:.2f} | OOS n={r[1]['n']:5d} {r[1]['abs_bp']:+5.0f} (t{r[1]['abs_t']:+.1f}) / {r[1]['neu_bp']:+5.0f} (t{r[1]['neu_t']:+.1f}) 勝率 {r[1]['win']:.2f}")

# 籠（売り、最大 30 枠、絶対リターン）
def basket(conf, h):
    ep = entry_pos(conf); sel = d1.confirm.values if conf else np.ones(len(d1), bool)
    ent = {}
    for cd, p, s in zip(code, ep, sel):
        if s: ent.setdefault(int(p), []).append(cd)
    daily = np.zeros(len(tdays)); open_pos = []
    for p in range(len(tdays)):
        open_pos = [(cd, e) for cd, e in open_pos if p < e + h]
        for cd in ent.get(p, []):
            if len(open_pos) < SLOTS: open_pos.append((cd, p))
        r = 0.0; w = 1.0 / SLOTS
        for cd, e in open_pos:
            if p == e: a, b = adjo.get((cd, p), np.nan), adjc.get((cd, p), np.nan); rr = -(b / a - 1) - COST
            else: a, b = adjc.get((cd, p - 1), np.nan), adjc.get((cd, p), np.nan); rr = -(b / a - 1)
            if p == e + h - 1: rr -= COST
            rr -= LEND / 245
            r += w * (0 if np.isnan(rr) else rr)
        daily[p] = r
    s = pd.Series(daily, index=tdays); return s[s.index >= d1.rdate.min()]

print("\n== 籠（売り、最大 30 枠、絶対リターン。比較は TOPIX 買い持ち）")
grid = []
for conf, h in itertools.product([True, False], [10, 20, 40]):
    s = basket(conf, h); a, o = split(s); sa, so = stats(a), stats(o)
    lab = f"確認{'あり' if conf else 'なし'} {h}日"
    print(f"  {lab:12s} IS {fmt(sa)} | OOS {fmt(so)}")
    grid.append(dict(label=lab, is_cagr=sa['cagr'], is_dd=sa['dd'], oos_cagr=so['cagr'], oos_dd=so['dd']))
    for per, x in [('IS', sa), ('OOS', so)]: rows.append(dict(kind='basket', confirm=conf, hold=h, period=per, **x))
tpr = tp.close.pct_change(); tpr = tpr[tpr.index >= d1.rdate.min()]; ta, to = split(tpr)
print(f"  {'TOPIX':12s} IS {fmt(stats(ta))} | OOS {fmt(stats(to))}")
for per, x in [('IS', stats(ta)), ('OOS', stats(to))]: rows.append(dict(kind='basket', confirm=None, hold=0, period=per, **x))
g = pd.DataFrame(grid)
print(f"  格子 6 の中央値: IS CAGR {g.is_cagr.median():.2f}% DD {g.is_dd.median():.1f}% | OOS CAGR {g.oos_cagr.median():.2f}% DD {g.oos_dd.median():.1f}%")
pd.DataFrame(rows).to_csv(f'{OUT}/t3_short.csv', index=False); print('saved')
