# N8. 優待クロスの前提検証。「株価変動を相殺すれば負けない」の唯一の穴は貸株料・逆日歩。
# 権利付き最終日（月末最終営業日）の前後で、貸借の規制・注意喚起がどれだけ増えるかを測る。
# 逆日歩そのもののデータは無いので、貸借規制（品薄の公的な指標）を代理変数にする。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
c.execute(f"CREATE VIEW ma AS SELECT * FROM read_parquet('{ROOT}/markets_margin_alert/*.parquet', union_by_name=true)")
c.execute(f"CREATE VIEW mi AS SELECT * FROM read_parquet('{ROOT}/markets_margin_interest/*.parquet', union_by_name=true)")
tp = topix(c); tdays = tp.index
per = tdays.to_period('M')
bwd = pd.Series(tdays, index=tdays).groupby(per).cumcount(ascending=False) + 1   # 月末から何営業日目
pos = pd.Series(bwd.values, index=tdays)

raw = c.execute("SELECT PubDate, Code, PubReason FROM ma").df()
raw['PubDate'] = pd.to_datetime(raw.PubDate)
for f in ['RestrictedByJSF', 'PrecautionByJSF']:
    raw[f] = raw.PubReason.str.extract(f'["\']{f}["\']: ["\'](\\d)', expand=False).astype(float)
raw = raw[raw.PubDate.isin(tdays)]
raw['bwd'] = raw.PubDate.map(pos)
raw['month'] = raw.PubDate.dt.month

# その日に貸借の規制・注意喚起が出ている銘柄数（1 日あたり）
g = raw.groupby('PubDate').agg(reg=('RestrictedByJSF', 'sum'), pre=('PrecautionByJSF', 'sum'))
g['bwd'] = g.index.map(pos); g['month'] = g.index.month
print('=== 月末から k 営業日目ごとの、貸借規制・注意喚起の銘柄数（1 日あたりの平均）===')
print(f'{"k":>3} {"日数":>5} {"貸借規制":>9} {"注意喚起":>9} {"合計":>9} {"全体比":>8}')
base = g[['reg', 'pre']].sum(axis=1).mean()
for k in range(1, 9):
    s = g[g.bwd == k]
    tot = (s.reg + s.pre).mean()
    print(f'{k:>3} {len(s):>5} {s.reg.mean():>9.1f} {s.pre.mean():>9.1f} {tot:>9.1f} {tot/base*100:>7.0f}%')
print(f'{"全体":>3} {len(g):>5} {g.reg.mean():>9.1f} {g.pre.mean():>9.1f} {base:>9.1f} {100:>7.0f}%')

print('\n=== 3 月・9 月（優待と配当が最も集中する月）に絞る ===')
print(f'{"k":>3} {"日数":>5} {"貸借規制":>9} {"注意喚起":>9} {"合計":>9} {"全体比":>8}')
h = g[g.month.isin([3, 9])]
for k in range(1, 9):
    s = h[h.bwd == k]
    if len(s) == 0: continue
    tot = (s.reg + s.pre).mean()
    print(f'{k:>3} {len(s):>5} {s.reg.mean():>9.1f} {s.pre.mean():>9.1f} {tot:>9.1f} {tot/base*100:>7.0f}%')

# --- 制度信用の売り残（週次）が権利週にどれだけ膨らむか ---
mi_df = c.execute("""SELECT Date AS d, Code,
    TRY_CAST(ShortMarginOutstanding AS DOUBLE) AS s,
    TRY_CAST(LongMarginOutstanding AS DOUBLE) AS l FROM mi""").df() if False else None
cols = c.execute('describe select * from mi').df().column_name.tolist()
print('\nmargin_interest の列:', ', '.join(cols))
