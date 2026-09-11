# 段階 11: 気配のずれは深さに比例する。実測（2026-09-11 9:00）で較正して順位付けを測り直す。
#   真のギャップ  ≤−7%   −7〜−5  −5〜−3  −3〜−1  −1〜0
#   ずれ平均      +9.12  +3.88   +1.84   +0.59   +0.39  （%ポイント、標準偏差 3.93/0.61/0.66/0.52/0.70）
# 深いほど大きくずれるので、観測ギャップは真の順位を**反転**させる（−7% は観測上プラスになり
# 候補から外れる）。一律のずれを仮定した段階 10 は害を過小評価していた。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

KNOT_G = np.array([-9.0, -8.0, -6.0, -4.0, -2.0, -0.5, 0.0])
KNOT_E = np.array([11.0,  9.12, 3.88, 1.84, 0.59, 0.39, 0.39])
KNOT_S = np.array([ 3.93, 3.93, 0.61, 0.66, 0.52, 0.70, 0.70])
COST, N, BUDGET, T, TRIALS = 5.7, 3, 1_000_000, '09:01', 20
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
mu = np.interp(df.gap_pct, KNOT_G, KNOT_E)
sd = np.interp(df.gap_pct, KNOT_G, KNOT_S)

def run(label, pool, obs_fn, trials=1):
    res = []
    for _ in range(trials):
        d = df.assign(obs=obs_fn())
        d = d[pool(d) & (d.obs < 0)].assign(kh=lambda x: (x.obs / 100) / x.vol20)
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

true_g = lambda: df.gap_pct.values
board = lambda: df.gap_pct.values + np.where(df.un, mu + sd * rng.standard_normal(len(df)), 0.0)
fixed = lambda: df.gap_pct.values + np.where(df.un, (mu + sd * rng.standard_normal(len(df))) * 0.0 + 1.089, 0.0)
allp, opened, un = (lambda d: pd.Series(True, index=d.index)), (lambda d: ~d.un), (lambda d: d.un)
print(f"495 営業日・決算除外・9:01 成行・往復 {COST} bp 込み\n")
run('① 上限: 真のギャップで全母集団', allp, true_g)
run('② 改修前: 寄済みだけ', opened, true_g)
run('③ 改修後: 全母集団・気配（深さ比例のずれ）', allp, board, TRIALS)
run('④ 参考: 全母集団・気配（一律のずれと仮定）', allp, fixed, TRIALS)
run('⑤ 未寄付だけ・気配（skip_opened = true）', un, board, TRIALS)

# 段階 12: ずれは深さの決まった関数なので、観測ギャップから真のギャップを逆算できるはず。
# 前向きの写像 g → g + mu(g) をグリッドで作り、単調な区間で反転する。
grid = np.linspace(-12, -0.01, 2000)
fwd = grid + np.interp(grid, KNOT_G, KNOT_E)
order = np.argsort(fwd)                       # 反転（写像は単調ではないので値でソートして最深を残す）
fwd_s, grid_s = fwd[order], grid[order]
def inverse(obs):
    return np.interp(obs, fwd_s, grid_s)
corrected = lambda: np.where(df.un, inverse(df.gap_pct.values + np.where(df.un, mu + sd*rng.standard_normal(len(df)), 0.0)), df.gap_pct.values)
run('⑥ 全母集団・気配を較正で補正', allp, corrected, TRIALS)
