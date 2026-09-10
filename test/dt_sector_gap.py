# 格下げの検証 その 2。業種まるごとのギャップダウンは戻らないのか。
# 2026-09-10 は建てた 4 銘柄のうち 2 つが建設業（清水建設・鹿島建設）で、どちらも前日に上げた
# 直後のギャップダウン。証券会社がセクターごと格下げした形。同業他社のギャップは 9:01 の時点で
# 観測できる（全銘柄の寄値が出ている）ので、規則として使える。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

MIN_TURNOVER = 1e8   # 業種の中央値を取る母集団（universe.min_turnover と同じ 1 億円）
c = con()

t = pd.read_csv(f'{OUT}/trades_now.csv'); t['date'] = pd.to_datetime(t.date); t['code'] = t.code.astype(str)
t['bp'] = t.pnl / t.amount * 1e4
L = t[t.side == 'long'].copy()

sec = c.execute("""
WITH m AS (SELECT Code, S33 FROM master WHERE Date = (SELECT max(Date) FROM master) AND S33 IS NOT NULL),
     b AS (SELECT Date, Code, AdjO, AdjC, Vo,
                  LAG(AdjC) OVER (PARTITION BY Code ORDER BY Date) pc
           FROM bars WHERE AdjO > 0 AND AdjC > 0)
SELECT b.Date, m.S33,
       median(b.AdjO / b.pc - 1) AS sec_gap,
       count(*) AS n_sec,
       sum(CASE WHEN b.AdjO / b.pc - 1 <= -0.02 THEN 1 ELSE 0 END) AS n_down
FROM b JOIN m ON m.Code = b.Code
WHERE b.pc > 0 AND b.AdjC * b.Vo >= {}
GROUP BY 1, 2 HAVING count(*) >= 5
""".format(int(MIN_TURNOVER))).df()
sec['Date'] = pd.to_datetime(sec.Date)
codes = c.execute("SELECT Code, S33, CoName FROM master WHERE Date = (SELECT max(Date) FROM master)").df()
codes['Code'] = codes.Code.astype(str)

L = L.merge(codes.rename(columns={'Code': 'code'}), on='code', how='left')
L = L.merge(sec.rename(columns={'Date': 'date'}), on=['date', 'S33'], how='left')
print(f"ロングの取引 {len(L):,} 件（{L.date.min():%Y-%m-%d}〜{L.date.max():%Y-%m-%d}）  業種を当てられた {L.sec_gap.notna().sum():,} 件")
L = L.dropna(subset=['sec_gap']).copy()
L['rel_gap'] = L.gap - L.sec_gap          # 業種に対する個別のギャップの深さ（負ほど個別要因）
L['down_ratio'] = L.n_down / L.n_sec      # 同業のうち −2% 以上ギャップダウンした割合
L['IS'] = L.date <= IS_END

def band(sub, col, labels=('低', '中', '高')):
    s = sub.copy()
    try: s['q'] = pd.qcut(s[col], 3, labels=list(labels), duplicates='drop')
    except ValueError: return "（分位を作れない）"
    a = s.groupby('q', observed=True).bp.agg(['size', 'mean'])
    a['t'] = s.groupby('q', observed=True).bp.apply(lambda x: x.mean() / x.std() * np.sqrt(len(x)))
    return "  ".join(f"{k} {r['mean']:+6.1f}bp (t {r.t:+4.1f}, n {int(r['size'])})" for k, r in a.iterrows())

print("\n=== (1) 3 分位ごとの平均 net bp")
for col, name in [('sec_gap', '業種の中央値ギャップ'), ('rel_gap', '業種に対する相対ギャップ'),
                  ('down_ratio', '同業の下げ広がり')]:
    print(f"  {name:<12} 全体  {band(L, col)}")
    for lab, sub in [('IS 〜2022', L[L.IS]), ('OOS 2023〜', L[~L.IS])]:
        print(f"  {'':<12} {lab:<5} {band(sub, col)}")

print("\n=== (2) 業種まるごと下げている日の取引を外すと")
for thr in (-0.005, -0.01, -0.015, -0.02):
    hit = L[L.sec_gap <= thr]; rest = L[L.sec_gap > thr]
    if len(hit) < 20: print(f"  業種の中央値ギャップ ≤ {thr:+.1%}: 該当 {len(hit)} 件（少なすぎ）"); continue
    d = hit.bp.mean() - rest.bp.mean()
    tt = d / np.sqrt(hit.bp.var() / len(hit) + rest.bp.var() / len(rest))
    isb = hit[hit.IS].bp.mean(); oob = hit[~hit.IS].bp.mean()
    print(f"  業種 ≤ {thr:+.1%}: 該当 {len(hit):4d} 件 {hit.bp.mean():+6.1f}bp (IS {isb:+6.1f} / OOS {oob:+6.1f})"
          f"  他 {rest.bp.mean():+6.1f}bp  差 {d:+6.1f} (t {tt:+4.1f})  該当の損益 {hit.pnl.sum():+,.0f} 円")

print("\n=== (3) 同じ日に同じ業種を 2 銘柄以上건てた日（セクター一括の材料の形）")
L['dup'] = L.groupby(['date', 'S33']).code.transform('size') >= 2
for lab, sub in [('全体', L), ('IS 〜2022', L[L.IS]), ('OOS 2023〜', L[~L.IS])]:
    a, b = sub[sub.dup].bp, sub[~sub.dup].bp
    d = a.mean() - b.mean(); tt = d / np.sqrt(a.var() / len(a) + b.var() / len(b))
    print(f"  {lab:<10} 同業重複 {a.mean():+6.1f}bp (n {len(a)})  単独 {b.mean():+6.1f}bp (n {len(b)})  差 {d:+6.1f} (t {tt:+4.1f})")
print(f"  同業重複の取引の損益合計 {L[L.dup].pnl.sum():+,.0f} 円")

print("\n=== (4) 前日に上げた × 業種も下げている（格下げらしさの複合）")
comb = L[(L.sec_gap <= -0.005)].copy()
prev = c.execute("""SELECT Date, Code, AdjC / LAG(AdjC) OVER (PARTITION BY Code ORDER BY Date) - 1 AS pr
                    FROM bars WHERE AdjC > 0""").df()
prev['Date'] = pd.to_datetime(prev.Date); prev['Code'] = prev.Code.astype(str)
prev = prev.rename(columns={'Date': 'pdate', 'Code': 'code'})
days = np.sort(L.date.unique()); nxt = dict(zip(days[:-1], days[1:]))
allday = np.sort(prev.pdate.unique()); nxtall = dict(zip(allday[:-1], allday[1:]))
prev['date'] = prev.pdate.map(nxtall)
L2 = L.merge(prev[['date', 'code', 'pr']], on=['date', 'code'], how='left').dropna(subset=['pr'])
for lab, sub in [('全体', L2), ('IS', L2[L2.IS]), ('OOS', L2[~L2.IS])]:
    hit = sub[(sub.pr > 0) & (sub.sec_gap <= -0.005)]; rest = sub.drop(hit.index)
    if len(hit) < 20: print(f"  {lab}: 該当 {len(hit)} 件（少なすぎ）"); continue
    d = hit.bp.mean() - rest.bp.mean(); tt = d / np.sqrt(hit.bp.var() / len(hit) + rest.bp.var() / len(rest))
    print(f"  {lab:<5} 前日上げ かつ 業種 ≤ −0.5%: 該当 {len(hit):4d} 件 {hit.bp.mean():+6.1f}bp"
          f"  他 {rest.bp.mean():+6.1f}bp  差 {d:+6.1f} (t {tt:+4.1f})  損益 {hit.pnl.sum():+,.0f} 円")

print("\n=== (5) 2026-09-10 の 4 銘柄")
d = L[L.date == '2026-09-10'][['code', 'CoName', 'S33Nm' if 'S33Nm' in L.columns else 'S33', 'gap', 'sec_gap', 'rel_gap', 'down_ratio', 'bp', 'pnl']]
print(d.to_string(index=False))

print("\n=== (6) 交絡の統制: 市場のギャップ・銘柄のギャップの帯の中で見ても同業重複は劣るか")
mkt = c.execute("""SELECT Date, O / LAG(C) OVER (ORDER BY Date) - 1 AS mgap FROM topix WHERE O > 0""").df()
mkt['Date'] = pd.to_datetime(mkt.Date)
L = L.merge(mkt.rename(columns={'Date': 'date'}), on='date', how='left')
L['mq'] = pd.qcut(L.mgap, 3, labels=['市場↓', '市場→', '市場↑'])
L['gq'] = pd.qcut(L.gap, 3, labels=['浅', '中', '深'])
for key, name in [('mq', '市場のギャップ'), ('gq', '銘柄のギャップ')]:
    print(f"  {name}の帯の中で:")
    for k, sub in L.groupby(key, observed=True):
        a, b = sub[sub.dup].bp, sub[~sub.dup].bp
        if len(a) < 20: print(f"    {k}: 重複 {len(a)} 件（少なすぎ）"); continue
        d = a.mean() - b.mean(); tt = d / np.sqrt(a.var() / len(a) + b.var() / len(b))
        print(f"    {k}  重複 {a.mean():+6.1f}bp (n {len(a):4d})  単独 {b.mean():+6.1f}bp (n {len(b):4d})  差 {d:+6.1f} (t {tt:+4.1f})")

print("\n=== (7) 重複の中身: 1 番目（ギャップが深い方）と 2 番目以降で違うか")
L['ord'] = L.sort_values(['date', 'S33', 'gap']).groupby(['date', 'S33']).cumcount()
dup = L[L.dup]
for k, sub in dup.groupby('ord'):
    if len(sub) < 20: continue
    print(f"  同業の {k+1} 番目（ギャップが深い順）: {sub.bp.mean():+6.1f}bp (n {len(sub):4d}, 損益 {sub.pnl.sum():+,.0f} 円)")
print(f"  参考: 単独の取引 {L[~L.dup].bp.mean():+.1f}bp (n {len(L[~L.dup]):,})")

print("\n=== (8) 業種を分散させたら何件が入れ替わるか（1 業種 1 銘柄にする場合）")
drop = dup[dup.ord >= 1]
print(f"  落とす取引 {len(drop):,} 件 / 全 {len(L):,} 件（{len(drop)/len(L):.1%}）  その損益 {drop.pnl.sum():+,.0f} 円  平均 {drop.bp.mean():+.1f}bp")
print(f"  残る取引の平均 {L.drop(drop.index).bp.mean():+.1f}bp")
print("  ※ 実際には次点が繰り上がるので、この差がそのまま損益になるわけではない（backtest で確かめる）")
