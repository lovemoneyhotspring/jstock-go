# 案 3. TOPIX 中立の悪決算ショート籠。t3_short の籠（確認なし、最大 30 枠）に同額の TOPIX 買いを重ねる。費用: 片道 0.10%、貸株料 1.1%、逆日歩見込み 2%/年。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
COST = 0.001; LEND = 0.011 + 0.02; SLOTS = 30
c = con(); tp = topix(c); tdays = tp.index; tidx = pd.Series(np.arange(len(tdays)), index=tdays)
u = pd.read_parquet(f'{OUT}/a_events.parquet'); u['ym'] = u.rdate.dt.to_period('M')
u['dec'] = u.groupby('ym').ear.transform(lambda s: pd.qcut(s.rank(method='first'), 10, labels=False) + 1 if len(s) >= 20 else np.nan)
d1 = u[u.dec == 1].copy()
mg = c.execute("SELECT Date, Code FROM master WHERE Mrgn='2'").df(); mg['Date'] = pd.to_datetime(mg.Date); mgset = set(zip(mg.Date, mg.Code))
d1 = d1[[(d, cd) in mgset for d, cd in zip(d1.rdate, d1.Code)]].copy()
codes = tuple(d1.Code.unique())
px = c.execute(f"SELECT Code, Date, AdjO, AdjC FROM bars WHERE Code IN {codes} AND AdjC>0").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]; px['pos'] = tidx.reindex(px.Date).values
key = px.set_index(['Code', 'pos']); adjc = key.AdjC.to_dict(); adjo = key.AdjO.to_dict()
code = d1.Code.values; rp = d1.rpos.values + 1
tpo = tp.open.values; tpc = tp.close.values
def basket(h):
    ent = {}
    for cd, p in zip(code, rp): ent.setdefault(int(p), []).append(cd)
    short = np.zeros(len(tdays)); hedge = np.zeros(len(tdays)); expo = np.zeros(len(tdays)); open_pos = []
    for p in range(len(tdays)):
        open_pos = [(cd, e) for cd, e in open_pos if p < e + h]
        for cd in ent.get(p, []):
            if len(open_pos) < SLOTS: open_pos.append((cd, p))
        r = 0.0; w = 1.0 / SLOTS; n_new = 0; n_out = 0
        for cd, e in open_pos:
            if p == e: a, b = adjo.get((cd, p), np.nan), adjc.get((cd, p), np.nan); rr = -(b / a - 1) - COST; n_new += 1
            else: a, b = adjc.get((cd, p - 1), np.nan), adjc.get((cd, p), np.nan); rr = -(b / a - 1)
            if p == e + h - 1: rr -= COST; n_out += 1
            rr -= LEND / 245
            r += w * (0 if np.isnan(rr) else rr)
        short[p] = r; expo[p] = len(open_pos) * w
        # ヘッジ: 建玉と同額の TOPIX を持つ。新規分は寄りから、既存分は前日引けから。費用 片道 0.02%（1306）
        r_new = (tpc[p] / tpo[p] - 1) if n_new else 0; r_old = (tpc[p] / tpc[p - 1] - 1) if p > 0 else 0
        hedge[p] = w * (n_new * (r_new - 0.0002) + (len(open_pos) - n_new) * r_old) - w * n_out * 0.0002
    s = pd.DataFrame(dict(short=short, hedge=hedge, expo=expo), index=tdays); return s[s.index >= d1.rdate.min()]
dt = pd.read_csv(f'{OUT}/trades_10y.csv'); dt['date'] = pd.to_datetime(dt.date); dtd = dt.groupby('date').pnl.sum()
rows = []
print("== 籠（売り最大 30 枠 + 同額 TOPIX 買い）。費用: 売買片道 0.10%、貸株料 1.1% + 逆日歩見込み 2%/年")
for h in [10, 20, 40]:
    s = basket(h); neu = s.short + s.hedge
    for name, r in [('売りのみ', s.short), ('TOPIX 中立', neu)]:
        a, o = split(r); sa, so = stats(a), stats(o)
        yr = r.groupby(r.index.year).sum() * 100
        corr = r.reindex(dtd.index).corr(dtd / 2e6)
        rows.append(dict(hold=h, kind=name, period='IS', **sa)); rows.append(dict(hold=h, kind=name, period='OOS', **so))
        print(f"  {h:2d}日 {name:9s} IS {fmt(sa)} | OOS {fmt(so)} | 平均露出 {s.expo.mean():.2f} | daytrade 相関 {corr:+.2f}")
        if name == 'TOPIX 中立': print("     年別(%):", {k: round(v, 1) for k, v in yr.items()})
tpr = tp.close.pct_change(); tpr = tpr[tpr.index >= d1.rdate.min()]; ta, to = split(tpr)
print(f"  TOPIX       IS {fmt(stats(ta))} | OOS {fmt(stats(to))}")
pd.DataFrame(rows).to_csv(f'{OUT}/p3_neutral_short.csv', index=False)
