# 格下げ（アナリストの投資判断の引き下げ）直後の銘柄を避けられるか。
# レーティングそのものは J-Quants に無いので、寄付（9:01）の時点で分かる価格・出来高の
# 代理変数で「前日引け後に個別の悪材料が出た形」を切り出せるかを見る。
# 使う変数は前日までの足と当日のギャップだけ（当日の出来高は寄付では未知なので使わない）。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

TRADES = f'{OUT}/trades_now.csv'
c = con()
t = pd.read_csv(TRADES); t['date'] = pd.to_datetime(t.date); t['code'] = t.code.astype(str)
t['bp'] = t.pnl / t.amount * 1e4
L = t[t.side == 'long'].copy()
print(f"ロングの取引 {len(L):,} 件（{L.date.min():%Y-%m-%d}〜{L.date.max():%Y-%m-%d}）  合計 {L.pnl.sum():,.0f} 円")

# 前日までの足から代理変数を作る（当日の情報は gap だけ）
px = c.execute("SELECT Date, Code, AdjC, Vo FROM bars WHERE AdjC>0").df()
px['Date'] = pd.to_datetime(px.Date); px['Code'] = px.Code.astype(str)
px = px.sort_values(['Code', 'Date'])
g = px.groupby('Code', sort=False)
px['ret1'] = g.AdjC.pct_change()                       # その日の終値リターン
px['ret5'] = g.AdjC.pct_change(5)
px['volmed'] = g.Vo.transform(lambda s: s.rolling(20, min_periods=10).median())
px['volrat'] = px.Vo / px.volmed
# 建てる日 d の「前日」の行を当てる（date を 1 営業日ずらす）
px['next'] = g.Date.shift(-1)
prev = px.dropna(subset=['next'])[['next', 'Code', 'ret1', 'ret5', 'volrat']]
prev.columns = ['date', 'code', 'prev_ret', 'prev_ret5', 'prev_volrat']
L = L.merge(prev, on=['date', 'code'], how='left')
print(f"  前日の足を当てられた取引 {L.prev_ret.notna().sum():,}/{len(L):,}")
L = L.dropna(subset=['prev_ret', 'prev_volrat'])
L['IS'] = L.date <= IS_END

def band(sub, col, labels=('低', '中', '高')):
    s = sub.copy(); s['q'] = pd.qcut(s[col], 3, labels=list(labels))
    a = s.groupby('q', observed=True).bp.agg(['size', 'mean'])
    a['t'] = s.groupby('q', observed=True).bp.apply(lambda x: x.mean() / x.std() * np.sqrt(len(x)))
    return "  ".join(f"{k} {r['mean']:+6.1f}bp (t {r.t:+4.1f}, n {int(r['size'])})" for k, r in a.iterrows())

print("\n=== (1) 3 分位ごとの平均 net bp（建てた日の損益／建玉）")
for col, name in [('prev_ret', '前日リターン'), ('prev_ret5', '前 5 日リターン'),
                  ('prev_volrat', '前日出来高倍率'), ('gap', 'ギャップ（参考）')]:
    print(f"  {name:<14} 全体  {band(L, col)}")
    for lab, sub in [('IS 〜2022', L[L.IS]), ('OOS 2023〜', L[~L.IS])]:
        print(f"  {'':<14} {lab:<5} {band(sub, col)}")

print("\n=== (2) 「格下げらしい形」= 前日に上げた直後のギャップダウン")
for thr in (0.0, 0.01, 0.02, 0.03):
    hit = L[L.prev_ret > thr]; rest = L[L.prev_ret <= thr]
    d = hit.bp.mean() - rest.bp.mean()
    tt = d / np.sqrt(hit.bp.var() / len(hit) + rest.bp.var() / len(rest))
    print(f"  前日 > {thr:+.0%}: 該当 {len(hit):4d} 件 {hit.bp.mean():+6.1f}bp  他 {len(rest):4d} 件 {rest.bp.mean():+6.1f}bp"
          f"  差 {d:+6.1f}bp (t {tt:+4.1f})  該当を外すと損益 {hit.pnl.sum():+,.0f} 円ぶん変わる")

print("\n=== (3) 前日に上げ、かつ前日の出来高も多い（材料が出ている形）")
for r_thr, v_thr in [(0.0, 1.5), (0.01, 1.5), (0.02, 1.5), (0.0, 2.0), (0.02, 2.0)]:
    hit = L[(L.prev_ret > r_thr) & (L.prev_volrat > v_thr)]; rest = L.drop(hit.index)
    if len(hit) < 20: print(f"  前日 > {r_thr:+.0%} かつ 出来高 > {v_thr}倍: 該当 {len(hit)} 件（少なすぎ）"); continue
    d = hit.bp.mean() - rest.bp.mean()
    tt = d / np.sqrt(hit.bp.var() / len(hit) + rest.bp.var() / len(rest))
    print(f"  前日 > {r_thr:+.0%} かつ 出来高 > {v_thr}倍: 該当 {len(hit):4d} 件 {hit.bp.mean():+6.1f}bp"
          f"  他 {rest.bp.mean():+6.1f}bp  差 {d:+6.1f}bp (t {tt:+4.1f})  該当の損益 {hit.pnl.sum():+,.0f} 円")

print("\n=== (4) 清水建設 2026-09-10 はどの帯にいたか")
s = L[(L.code == '18030') & (L.date == '2026-09-10')]
if len(s):
    r = s.iloc[0]
    print(f"  前日リターン {r.prev_ret:+.2%}  前 5 日 {r.prev_ret5:+.2%}  前日出来高倍率 {r.prev_volrat:.2f}"
          f"  ギャップ {r.gap:+.2%}  net {r.bp:+.1f}bp  損益 {r.pnl:+,.0f} 円")
else:
    print("  取引が見つからない（CSV の期間外）")
