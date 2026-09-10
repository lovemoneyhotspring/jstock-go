# A. 決算後ドリフト（PEAD）。決算短信への初動（超過）で十分位に分け、その後 5/10/20/40 営業日の超過を横断で検定。
# 上位十分位の籠（翌寄り買い・20 日保有・最大 30 銘柄・等金額）を TOPIX と比べる。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
HOLDS = [5, 10, 20, 40]
MAXH = max(HOLDS)

# --- 営業日の並び（TOPIX の日付） ---
tp = topix(c)
tdays = tp.index
tidx = pd.Series(np.arange(len(tdays)), index=tdays)

# --- 事象表 ---
ev = c.execute("""
SELECT Code, DiscDate, DiscTime, DocType, CurPerType, CurFYEn,
       TRY_CAST(NP AS DOUBLE) NP, TRY_CAST(FNP AS DOUBLE) FNP, TRY_CAST(NxFNp AS DOUBLE) NxFNp
FROM fins
WHERE DocType LIKE '%FinancialStatements%' AND DocType NOT LIKE '%REIT%' AND DocType NOT LIKE '%Foreign%'
  AND DocType NOT LIKE 'OtherPeriod%'
""").df()
ev['DiscDate'] = pd.to_datetime(ev.DiscDate)
ev['t'] = ev.DiscTime.fillna('').str.slice(0, 5)
close_t = np.where(ev.DiscDate >= '2024-11-05', '15:30', '15:00')
ev['after'] = (ev.t >= close_t) | (ev.t == '')
# 反応日 = 開示日（場中）or 開示日の次の営業日
pos = np.searchsorted(tdays.values, ev.DiscDate.values, side='left')  # 開示日以降の最初の営業日
same = (pos < len(tdays)) & (tdays.values[np.minimum(pos, len(tdays)-1)] == ev.DiscDate.values)
react_pos = np.where(same & ~ev.after.values, pos, pos + np.where(same, 1, 0))
ev['rpos'] = react_pos
ev = ev[(ev.rpos >= 1) & (ev.rpos + 1 + MAXH < len(tdays))].copy()
ev['rdate'] = tdays.values[ev.rpos.values]
# 同じ銘柄・同じ反応日が複数あれば（訂正など）最初の 1 件
ev = ev.sort_values(['Code', 'rdate', 'DiscDate']).drop_duplicates(['Code', 'rdate'])
print('events', len(ev), ev.rdate.min().date(), ev.rdate.max().date())

# --- 前年同期の NP / 前回の FNP（同じ CurFYEn） ---
ev['CurFYEn'] = pd.to_datetime(ev.CurFYEn)
prev = ev[['Code', 'CurPerType', 'CurFYEn', 'NP']].copy()
prev['CurFYEn'] = prev.CurFYEn + pd.DateOffset(years=1)
prev = prev.rename(columns={'NP': 'NP_yoy'}).drop_duplicates(['Code', 'CurPerType', 'CurFYEn'])
ev = ev.merge(prev, on=['Code', 'CurPerType', 'CurFYEn'], how='left')
ev = ev.sort_values(['Code', 'CurFYEn', 'DiscDate'])
ev['FNP_prev'] = ev.groupby(['Code', 'CurFYEn']).FNP.shift(1)

# --- 価格（AdjC / AdjO）を銘柄ごとに営業日の並びへ ---
codes = tuple(ev.Code.unique())
px = c.execute(f"SELECT Code, Date, AdjO, AdjC, C, MktCap, Va FROM bars WHERE Code IN {codes} AND AdjC>0 ORDER BY Code, Date").df()
px['Date'] = pd.to_datetime(px.Date)
px = px[px.Date.isin(tdays)]
px['pos'] = tidx.reindex(px.Date).values
px['va20'] = px.groupby('Code').Va.transform(lambda s: s.rolling(20, min_periods=10).mean())
key = px.set_index(['Code', 'pos'])
adjc = key.AdjC; adjo = key.AdjO; mcap = key.MktCap; va20 = key.va20
tpc = tp.close.values; tpo = tp.open.values

def get(s, code, p):
    return s.reindex(list(zip(code, p))).values

code = ev.Code.values; rp = ev.rpos.values
ev['c_pre'] = get(adjc, code, rp - 1)
ev['c_react'] = get(adjc, code, rp)
ev['o_entry'] = get(adjo, code, rp + 1)
ev['mcap'] = get(mcap, code, rp)
ev['va20'] = get(va20, code, rp)
ev['ear'] = (ev.c_react / ev.c_pre - 1) - (tpc[rp] / tpc[rp - 1] - 1)
ev['sue'] = (ev.NP - ev.NP_yoy) / (ev.mcap * 1e6)
ev['rev'] = np.where(ev.CurPerType == 'FY', ev.NxFNp / ev.NP.abs() - 1, ev.FNP / ev.FNP_prev.abs() - 1)
ev.loc[(ev.CurPerType == 'FY') & (ev.NP <= 0), 'rev'] = np.nan
for h in HOLDS:
    ev[f'x{h}'] = (get(adjc, code, rp + 1 + h) / ev.o_entry - 1) - (tpc[rp + 1 + h] / tpo[rp + 1] - 1)

# 母集団: 時価総額 100 億円以上、売買代金 20 日平均 1 億円以上、価格が揃っている
u = ev[(ev.mcap >= 10000) & (ev.va20 >= 1e8) & ev.c_pre.notna() & ev.o_entry.notna() & ev.x40.notna()].copy()
u['period'] = np.where(u.rdate <= IS_END, 'IS', 'OOS')
print('universe events', len(u), u.groupby('period').size().to_dict())
u.to_parquet(f'{OUT}/a_events.parquet')

# --- 横断検定: 反応月ごとの十分位 ---
def decile_table(df, sig, name):
    d = df[df[sig].notna()].copy()
    d['ym'] = d.rdate.dt.to_period('M')
    d['dec'] = d.groupby('ym')[sig].transform(lambda s: pd.qcut(s.rank(method='first'), 10, labels=False) + 1 if len(s) >= 20 else np.nan)
    d = d[d.dec.notna()]
    print(f"\n== 信号 {name}  n={len(d)}  （十分位 → 平均超過 bp と t 値、保有 20 日）")
    for per in ['IS', 'OOS']:
        x = d[d.period == per]
        g = x.groupby('dec')
        line = []
        for dec in [1, 2, 5, 9, 10]:
            y = x[x.dec == dec].x20
            line.append(f"D{int(dec)} {y.mean()*1e4:+6.0f}bp(t{y.mean()/y.std()*np.sqrt(len(y)):+.1f})")
        ls = x[x.dec == 10].x20.mean() - x[x.dec == 1].x20.mean()
        print(f"  {per:3s} " + '  '.join(line) + f"  D10-D1 {ls*1e4:+.0f}bp")
    print("  保有日数ごとの D10 平均超過 bp（IS / OOS）:", {h: (round(d[(d.dec==10)&(d.period=='IS')][f'x{h}'].mean()*1e4), round(d[(d.dec==10)&(d.period=='OOS')][f'x{h}'].mean()*1e4)) for h in HOLDS})
    # 年ごとの D10 の符号
    yr = d[d.dec == 10].groupby(d.rdate.dt.year).x20.mean() * 1e4
    print("  D10 年別 bp:", {k: int(v) for k, v in yr.items()})
    return d

d_ear = decile_table(u, 'ear', '初動 EAR（反応日の超過）')
d_sue = decile_table(u, 'sue', 'SUE（純利益の前年同期差 / 時価総額）')
d_rev = decile_table(u, 'rev', '会社予想の修正（FNP 前回比、FY は翌期予想/実績）')

# --- 籠: EAR の最上位十分位を翌寄りで買い、20 日保有、最大 30 銘柄・等金額、片道 0.10% ---
def basket(d, hold=20, maxn=30, cost=0.001):
    picks = d[d.dec == 10].sort_values('rdate')
    # 各営業日のポジション: (code, entry_pos, exit_pos)
    entries = {}
    for _, r in picks.iterrows():
        entries.setdefault(int(r.rpos) + 1, []).append(r.Code)
    ret = pd.Series(0.0, index=tdays)
    open_pos = []   # (code, entry_pos)
    nav = 1.0; navs = []
    daily = np.zeros(len(tdays))
    for p in range(len(tdays)):
        # 退出
        open_pos = [(cd, ep) for cd, ep in open_pos if p < ep + hold + 1]
        # 新規（枠が空いていれば）
        for cd in entries.get(p, []):
            if len(open_pos) < maxn: open_pos.append((cd, p))
        if not open_pos: daily[p] = 0; continue
        w = 1.0 / maxn
        r = 0.0
        for cd, ep in open_pos:
            if p == ep:      # 寄りで入って引けまで
                a, b = adjo.get((cd, p), np.nan), adjc.get((cd, p), np.nan); r += w * ((b / a - 1) - cost)
            elif p == ep + hold:   # 最終日、引けで出る
                a, b = adjc.get((cd, p - 1), np.nan), adjc.get((cd, p), np.nan); r += w * ((b / a - 1) - cost)
            else:
                a, b = adjc.get((cd, p - 1), np.nan), adjc.get((cd, p), np.nan); r += w * (b / a - 1)
        daily[p] = 0 if np.isnan(r) else r
    return pd.Series(daily, index=tdays)

bk = basket(d_ear)
bk = bk[bk.index >= u.rdate.min()]
tpr = tp.close.pct_change().reindex(bk.index)
print("\n== 籠（EAR D10、翌寄り買い 20 日保有、最大 30、片道 0.10%）")
for per, (a, b) in {'IS': split(bk)[0:1] and (split(bk)[0], split(tpr)[0]), 'OOS': (split(bk)[1], split(tpr)[1])}.items():
    print(f"  {per:3s} 籠   {fmt(stats(a))}\n      TOPIX {fmt(stats(b))}")
bk.to_csv(f'{OUT}/a_basket_daily.csv')
