# 段階 10: 気配で順位を付けたらどこまで届くか。
# 未寄付の銘柄の観測ギャップ = 真のギャップ + ずれ。ずれは 2026-09-11 の実測
# （9:00、824 銘柄、平均 +1.089%・標準偏差 1.885%）から作る。寄済みは真の値が見える。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

BIAS, SD = 1.089, 1.885     # 気配の中値 − 寄値（%ポイント）
COST, N, BUDGET, T = 5.7, 3, 1_000_000, '09:01'
TRIALS = 20
rng = np.random.default_rng(20260911)
c = con()
df = c.execute(f"""
 WITH e AS (SELECT DISTINCT Code, CAST(DiscDate AS DATE) d FROM fins),
 cal AS (SELECT Date, LAG(Date) OVER (ORDER BY Date) prev FROM (SELECT DISTINCT Date FROM bars))
 SELECT g.Date, g.Code, g.gap*100 gap_pct, g.open_time, g.vol20, g.open_px, p.px_0901, b.C close_raw
 FROM read_parquet('{OUT}/open_gap.parquet') g
 JOIN read_parquet('{OUT}/open_px_at.parquet') p ON p.Date=g.Date AND p.Code=g.Code
 JOIN bars b ON b.Date=g.Date AND b.Code=g.Code JOIN cal ON cal.Date=g.Date
 WHERE g.gap<0 AND b.C>0 AND g.open_px>0
   AND NOT EXISTS (SELECT 1 FROM e WHERE e.Code=g.Code AND e.d=CAST(g.Date AS DATE))
   AND NOT EXISTS (SELECT 1 FROM e WHERE e.Code=g.Code AND e.d=CAST(cal.prev AS DATE))""").df()
df['un'] = df.open_time > T
df['fill'] = np.where(df.un, df.open_px, df.px_0901)
df = df.dropna(subset=['fill'])

def run(label, pool_mask, bias, sd, trials=1):
    res = []
    for _ in range(trials):
        obs = df.gap_pct + np.where(df.un, bias + sd * rng.standard_normal(len(df)), 0.0)
        obs = np.minimum(obs, -0.0001)           # 観測上も下げでなければ買わない
        d = df.assign(kh=(obs / 100) / df.vol20, obs=obs)
        d = d[pool_mask(d) & (d.obs < 0)]
        rows = []
        for day, g in d.groupby('Date'):
            pick = g.nsmallest(N, 'kh')
            r = (pick.close_raw / pick.fill - 1) * 1e4 - COST
            rows.append(dict(n=len(pick), bp=r.mean(), gap=pick.gap_pct.mean(), pnl=(r / 1e4 * BUDGET).sum()))
        s = pd.DataFrame(rows)
        res.append((s.bp.mean(), s.pnl.sum(), s.n.mean(), s.gap.mean(), len(s)))
    a = np.array(res)
    print(f"  {label:<40} 日 {a[:,4].mean():.0f}  銘柄/日 {a[:,2].mean():.1f}  ギャップ {a[:,3].mean():+6.2f}%  "
          f"net {a[:,0].mean():+6.1f} bp  2 年 {a[:,1].mean():>10,.0f} 円")

allp = lambda d: pd.Series(True, index=d.index)
print(f"495 営業日・決算除外・9:01 成行・1 銘柄 100 万円 × {N}・往復 {COST} bp 込み\n")
print("■ 上限と下限")
run('全母集団・真のギャップ（到達不能な上限）', allp, 0.0, 0.0)
run('寄済みだけ（改修前の実運用）', lambda d: ~d.un, 0.0, 0.0)
print(f"\n■ 気配で順位を付ける（ずれ 平均 {BIAS:+.2f}%・標準偏差 {SD:.2f}%、{TRIALS} 回の平均）")
run('全母集団（気配をそのまま使う＝今回の改修）', allp, BIAS, SD, TRIALS)
run('全母集団（平均のずれだけ補正）', allp, 0.0, SD, TRIALS)
run('未寄付だけ・気配（skip_opened = true）', lambda d: d.un, BIAS, SD, TRIALS)
