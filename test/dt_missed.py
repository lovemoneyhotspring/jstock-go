# 敗因分析の型: 9:01 の候補と「理想の候補」の差を測る。
#
# 実運用の順位表（history/ranking、ranking_source = quotes）には、その時刻に**気配が取れた
# 銘柄しか載らない**。2026-09-11 に分かったとおり、寄り前・未寄付の銘柄は始値も現在値も
# 空なので値段が取れず、候補から丸ごと消えていた（docs/OPENING_DATA.md「実機で確かめること」3）。
# 消えるのは寄付が遅れる銘柄＝利益源なので、ここが最大の敗因になりうる。
#
# この型が出すのは 2 つ:
#   1. 欠け      … 前夜の母集団のうち 9:01 の順位表に載らなかった銘柄
#   2. 逃した利益 … そのうちギャップが建てる条件を満たしていた銘柄の、当日の寄→引
#
# 2 は「建てていれば取れた」ではない（同じ資金で別の銘柄を建てている）。**順位の上位に
# 食い込んだはずの銘柄がどちらに転んだか**を見て、取りこぼしの向きを測るためのもの。
#
#   python test/dt_missed.py 2026-09-11            # 1 日
#   python test/dt_missed.py 2026-09-01 2026-09-11 # 期間
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

STATE = os.path.expanduser('~/jstock-go/state/daytrade/history')
MAX_GAP = 0.0     # signal.max_gap（これ未満のギャップだけ買う）
N = 3             # capital: max_capital / order_budget

frm = sys.argv[1] if len(sys.argv) > 1 else None
to = sys.argv[2] if len(sys.argv) > 2 else frm
if not frm:
    print(__doc__ or 'usage: python test/dt_missed.py FROM [TO]'); sys.exit(1)

c = con()
c.execute(f"CREATE VIEW plan AS SELECT * FROM read_parquet('{STATE}/plan/*.parquet', union_by_name=true)")
c.execute(f"CREATE VIEW rank_hist AS SELECT * FROM read_parquet('{STATE}/ranking/*.parquet', union_by_name=true)")

# 前夜の母集団（eligible）× 当日の日足 → 理想のギャップ。9:01 の順位表に載ったかを突き合わせる。
# plan は 1 日に複数回走るので、その日の最後の run_id だけを使う。
df = c.execute(f"""
WITH p AS (
  SELECT * FROM plan WHERE eligible
    AND day BETWEEN DATE '{frm}' AND DATE '{to}'
    AND run_id = (SELECT run_id FROM plan p2 WHERE p2.day = plan.day ORDER BY recorded_at DESC LIMIT 1)),
first AS (  -- その日の最初の open（9:01）の順位表。後の回は寄った銘柄が増えるので欠けが埋もれる
  SELECT day, arg_min(run_id, recorded_at) AS run_id FROM rank_hist
  WHERE day BETWEEN DATE '{frm}' AND DATE '{to}' AND hour(recorded_at AT TIME ZONE 'Asia/Tokyo') = 9
  GROUP BY day),
r AS (SELECT DISTINCT h.day, h.symbol FROM rank_hist h JOIN first f ON f.day = h.day AND f.run_id = h.run_id),
b AS (SELECT Date, Code, O, C FROM bars WHERE Date BETWEEN DATE '{frm}' AND DATE '{to}' AND O > 0 AND C > 0)
SELECT p.day, p.symbol, p.name, p.vol20,
       b.O AS open, b.C AS close,
       b.O / p.prev_close - 1 AS gap,
       (b.O / p.prev_close - 1) / NULLIF(p.vol20, 0) AS gap_vol,
       (b.C / b.O - 1) * 1e4 AS oc_bp,
       (r.symbol IS NOT NULL) AS ranked
FROM p JOIN b ON b.Code = p.Code AND b.Date = p.day
       LEFT JOIN r ON r.day = p.day AND r.symbol = p.symbol
""").df()

if df.empty:
    print('突き合わせる記録がありません（9:01 の順位表は ranking_source = quotes の日だけ）'); sys.exit(0)

for day, g in df.groupby('day'):
    cand = g[g.gap < MAX_GAP].sort_values('gap_vol')   # 買う条件を満たす候補を理想の順で
    missed = cand[~cand.ranked]
    top = cand.head(N)
    print(f"\n=== {day:%Y-%m-%d}  母集団 {len(g)}  9:01 の順位表に載った {int(g.ranked.sum())}"
          f"  欠け {int((~g.ranked).sum())}")
    print(f"  買う条件を満たす候補 {len(cand)}  うち欠け {len(missed)}"
          f"  理想の上位 {N} のうち欠け {int((~top.ranked).sum())}")
    if len(missed):
        print(f"  欠けた候補の寄→引 平均 {missed.oc_bp.mean():+.1f} bp / 載った候補 {cand[cand.ranked].oc_bp.mean():+.1f} bp")
    for _, row in top.iterrows():
        mark = '  ' if row.ranked else '欠'
        print(f"   {mark} {row.symbol} {row['name'][:12]:<12} ギャップ {row.gap*100:+6.2f}%"
              f"  gap/vol {row.gap_vol:+6.2f}  寄→引 {row.oc_bp:+7.1f} bp")
