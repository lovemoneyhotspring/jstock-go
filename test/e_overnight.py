# E. オーバーナイト・プレミアム。夜間(C→翌O)と日中(O→C)に分解し、引け買い翌寄り売りを費用込みで買い持ちと比べる。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *

c = con()
series = {
    '1306(TOPIX ETF)': (jp_etf(c, '13060'), IS_END),
    '1321(N225 ETF)': (jp_etf(c, '13210'), IS_END),
    'TOPIX指数': (topix(c), IS_END),
    'SPY': (wb('SPY'), IS_END),
    '^N225(30y)': (wb('^N225'), LONG_IS_END),
    '^GSPC(30y)': (wb('^GSPC'), LONG_IS_END),
}
rows = []
for name, (df, is_end) in series.items():
    df = df[(df.open > 0) & (df.close > 0)].copy()
    night = df.open / df.close.shift(1) - 1   # 前日引け→当日寄り
    day = df.close / df.open - 1              # 当日寄り→引け
    full = df.close / df.close.shift(1) - 1
    print(f"\n== {name}  {df.index[0].date()}〜{df.index[-1].date()}")
    for label, r in [('買い持ち', full), ('夜間のみ(費用0)', night), ('日中のみ(費用0)', day)]:
        a, b = split(r, is_end)
        print(f"  {label:14s} IS {fmt(stats(a))} | OOS {fmt(stats(b))}")
        for per, x in [('IS', a), ('OOS', b)]:
            s = stats(x); rows.append(dict(series=name, strategy=label, cost_rt=0, period=per, **s))
    for cost in [0.02, 0.04, 0.06]:
        r = night - cost / 100
        a, b = split(r, is_end)
        print(f"  夜間 往復{cost:.2f}%    IS {fmt(stats(a))} | OOS {fmt(stats(b))}")
        for per, x in [('IS', a), ('OOS', b)]:
            s = stats(x); rows.append(dict(series=name, strategy='夜間', cost_rt=cost, period=per, **s))
    # 弱い仮説: 曜日・月末の偏り（費用なし、夜間の平均 bp）
    wd = night.groupby(night.index.dayofweek).mean() * 1e4
    me = night.groupby(night.index.is_month_end | (night.index.day >= 28)).mean() * 1e4
    print(f"  夜間 曜日平均bp(月〜金): {[round(v,1) for v in wd.values]}  月末(28日〜) {me.get(True, float('nan')):.1f} / 他 {me.get(False, float('nan')):.1f}")
    print(f"  夜間 t値(全期間): {night.mean()/night.std()*np.sqrt(len(night)):.2f}  日中 t値: {day.mean()/day.std()*np.sqrt(len(day)):.2f}")

pd.DataFrame(rows).to_csv(f'{OUT}/e_overnight.csv', index=False)
print('\nsaved', f'{OUT}/e_overnight.csv')
