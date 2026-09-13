# N9. TOB のサヤ取りを、価格と上場廃止から逆算する。
# 公開買付届出書は手元に無いが、TOB が成立した銘柄は「大きなギャップアップ → 買付価格に張り付き → 上場廃止」
# という形を必ず取る。最終売買日の株価 ≒ 買付価格なので、「跳ねた翌寄りで買い、最終売買日の引けで売る」で
# サヤの実測値が出る。撤回・不成立は「跳ねたのに上場廃止に至らなかった」群として同じ枠で数える。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
tp = topix(c); tdays = tp.index
px = c.execute("""SELECT Date, Code, AdjO, AdjC, TRY_CAST(Va AS DOUBLE) Va
                  FROM bars WHERE AdjC>0 AND AdjO>0 ORDER BY Date""").df()
px['Date'] = pd.to_datetime(px.Date); px = px[px.Date.isin(tdays)]
O = px.pivot(index='Date', columns='Code', values='AdjO').reindex(tdays)
C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
V = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays)
del px
codes = list(O.columns); T = len(tdays)
OP, CL = O.values, C.values
ADV = V.rolling(20).mean().values
DATA_END = tdays[-1]

# --- 上場廃止: 売買が途切れて戻らない銘柄。データ終端の 60 日前までに終わったものだけ ---
lastix = (~np.isnan(CL)).cumsum(axis=0).argmax(axis=0)
has = ~np.isnan(CL).all(axis=0)
cutoff = T - 60
delisted = {codes[j]: lastix[j] for j in range(len(codes)) if has[j] and lastix[j] < cutoff}
print('上場廃止（データ終端の 60 日以上前に売買が終わった銘柄）', len(delisted))

# --- 跳ね: 1 日の終値リターン ≥ +15%（TOB 公表の翌日はほぼ確実にストップ高かそれに近い）---
ret = CL[1:] / CL[:-1] - 1
JUMP = 0.15
rows = []
for j, code in enumerate(codes):
    if not has[j]: continue
    end = lastix[j]
    lo = max(1, end - 250)
    r = ret[lo - 1:end, j]
    if len(r) == 0 or np.all(np.isnan(r)): continue
    k = int(np.nanargmax(r))
    if not (r[k] >= JUMP): continue
    t = lo + k                      # 跳ねた日
    if ADV[t - 1, j] < 3e7: continue      # 直前 20 日平均 3,000 万円未満は除く
    ent = t + 1
    if ent >= T or np.isnan(OP[ent, j]): continue
    is_del = code in delisted
    exit_i = end if is_del else min(t + 120, T - 1)
    if exit_i <= ent or np.isnan(CL[exit_i, j]): continue
    rows.append(dict(code=code, d=tdays[t], jump=r[k], ent=ent, exit=exit_i,
                     days=exit_i - ent, delisted=is_del,
                     ret=CL[exit_i, j] / OP[ent, j] - 1, adv=ADV[t - 1, j]))
e = pd.DataFrame(rows)
print('跳ね（+15% 以上、直前 20 日平均 3,000 万円以上）', len(e),
      ' うち上場廃止に至った', int(e.delisted.sum()))

def tv(s): return s.mean() / (s.std(ddof=1) / np.sqrt(len(s))) if len(s) > 2 else np.nan
COST = 15.7   # 買って売るだけ（ヘッジ不要。価格は買付価格に固定される）

def show(name, sub):
    if len(sub) < 10: return
    net = sub.ret * 1e4 - COST
    i, o = net[sub.d <= IS_END], net[sub.d > IS_END]
    ann = (sub.ret - COST / 1e4) / sub.days * 245 * 100
    print(f'{name:<28} {len(sub):>5} {net.mean():>9.1f} {tv(net):>6.2f} {net.median():>9.1f} '
          f'{(net > 0).mean()*100:>5.1f}% {sub.days.median():>6.0f} {ann.median():>8.1f} '
          f'{i.mean() if len(i) else np.nan:>9.1f} {o.mean() if len(o) else np.nan:>9.1f}')

print(f'\n=== 跳ねた翌寄りで買い、最終売買日（または 120 日後）の引けで売る ===')
print(f'{"群":<28} {"件数":>5} {"純bp":>9} {"t":>6} {"中央値":>9} {"勝率":>6} {"日数":>6} {"年率%":>8} {"IS":>9} {"OOS":>9}')
show('上場廃止に至った（TOB 成立）', e[e.delisted])
show('  跳ね +30% 以上', e[e.delisted & (e.jump >= 0.30)])
show('  跳ね +15〜30%', e[e.delisted & (e.jump < 0.30)])
show('至らなかった（不成立・別要因）', e[~e.delisted])
show('全体', e)

print('\n=== 上場廃止に至った群: 跳ねの翌寄りから n 日後までの経過（母集団中立ではない生の値、bp）===')
d = e[e.delisted]
print(f'{"n":>4} {"件数":>5} {"平均":>9} {"中央値":>9}')
for n in (1, 5, 10, 20, 40, 60):
    v = []
    for _, r in d.iterrows():
        k = min(r.ent + n, r.exit)
        if np.isnan(CL[k, codes.index(r.code)]): continue
        v.append(CL[k, codes.index(r.code)] / OP[r.ent, codes.index(r.code)] - 1)
    if v: print(f'{n:>4} {len(v):>5} {np.mean(v)*1e4:>9.1f} {np.median(v)*1e4:>9.1f}')
print(f'\n保有日数の分布（上場廃止群）: 中央値 {d.days.median():.0f} 日  四分位 {d.days.quantile(.25):.0f}〜{d.days.quantile(.75):.0f} 日')
print(f'売買代金 20 日平均の中央値 {d.adv.median()/1e8:.2f} 億円')
print(f'年 {len(d)/((tdays[-1]-tdays[0]).days/365):.1f} 件')
