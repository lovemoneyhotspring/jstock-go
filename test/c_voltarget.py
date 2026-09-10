# C. インデックスのボラ目標。露出 = clamp(目標ボラ/実現ボラ, 下限, 上限) を日次で見直し、買い持ちと比べる。
import sys; sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *
import itertools

COST = 0.0002      # 片道
SPREAD = 0.01      # 借入 = ^IRX + 1%/年
THRESH = 0.05      # 露出の変化がこれ未満なら売買しない

def run(close, rf, target, win, cap, floor):
    r = close.pct_change()
    vol = r.rolling(win).std() * np.sqrt(245)
    want = (target / vol).clip(floor, cap)
    exp = pd.Series(np.nan, index=close.index)
    cur = np.nan; traded = pd.Series(0.0, index=close.index)
    w = want.values; e = np.empty(len(w)); t = np.zeros(len(w))
    for i in range(len(w)):
        if np.isnan(w[i]): e[i] = np.nan; continue
        if np.isnan(cur) or abs(w[i] - cur) >= THRESH:
            t[i] = abs(w[i] - (0 if np.isnan(cur) else cur)); cur = w[i]
        e[i] = cur
    exp = pd.Series(e, index=close.index).shift(1)     # 当日の露出は前日引けで決める
    borrow = ((exp - 1).clip(lower=0) * (rf.reindex(close.index).ffill().fillna(0) + SPREAD) / 245)
    strat = exp * r - borrow - pd.Series(t, index=close.index).shift(1).fillna(0) * COST
    return strat.dropna(), exp

def ma200(close):
    r = close.pct_change()
    sig = (close > close.rolling(200).mean()).astype(float).shift(1)
    trades = sig.diff().abs().fillna(0)
    return (sig * r - trades * COST).dropna()

c = con()
rf = irx()
series = {
    'SPY': (wb('SPY').close, IS_END),
    '1306': (jp_etf(c, '13060').close, IS_END),
    '^GSPC(30y)': (wb('^GSPC').close, LONG_IS_END),
    '^N225(30y)': (wb('^N225').close, LONG_IS_END),
}
grid = list(itertools.product([0.10, 0.12, 0.15], [20, 60], [1.5, 2.0], [0.0, 0.5]))
rows = []
for name, (close, is_end) in series.items():
    close = close[close > 0]
    bh = close.pct_change().dropna()
    # 助走 200 日は全戦略から外す（MA200 に合わせる）
    start = close.index[200]
    print(f"\n== {name}  {start.date()}〜{close.index[-1].date()}")
    a, b = split(bh[bh.index >= start], is_end)
    print(f"  {'買い持ち':22s} IS {fmt(stats(a))} | OOS {fmt(stats(b))}")
    for per, x in [('IS', a), ('OOS', b)]: rows.append(dict(series=name, strategy='買い持ち', period=per, **stats(x)))
    m = ma200(close); a, b = split(m[m.index >= start], is_end)
    print(f"  {'MA200フィルタ':22s} IS {fmt(stats(a))} | OOS {fmt(stats(b))}")
    for per, x in [('IS', a), ('OOS', b)]: rows.append(dict(series=name, strategy='MA200', period=per, **stats(x)))
    res = []
    for target, win, cap, floor in grid:
        s, exp = run(close, rf, target, win, cap, floor)
        s = s[s.index >= start]
        a, b = split(s, is_end)
        sa, sb = stats(a), stats(b)
        label = f"vt{int(target*100)} w{win} cap{cap} fl{floor}"
        res.append((label, sa, sb, exp[exp.index >= start].mean()))
        for per, x in [('IS', sa), ('OOS', sb)]: rows.append(dict(series=name, strategy=label, period=per, **x))
    df = pd.DataFrame([dict(label=l, is_cagr=sa['cagr'], is_dd=sa['dd'], oos_cagr=sb['cagr'], oos_dd=sb['dd'], avg_exp=e) for l, sa, sb, e in res])
    print(f"  格子 {len(df)} 通りの中央値: IS CAGR {df.is_cagr.median():.2f}% DD {df.is_dd.median():.1f}% | OOS CAGR {df.oos_cagr.median():.2f}% DD {df.oos_dd.median():.1f}%  平均露出 {df.avg_exp.median():.2f}")
    print(f"  IS で買い持ちに勝つ数: CAGR {(df.is_cagr > stats(a if False else bh[(bh.index>=start)&(bh.index<=is_end)])['cagr']).sum()}/{len(df)}")
    for _, r in df.sort_values('oos_cagr', ascending=False).head(3).iterrows():
        print(f"    {r.label:26s} IS {r.is_cagr:6.2f}% DD {r.is_dd:5.1f}% | OOS {r.oos_cagr:6.2f}% DD {r.oos_dd:5.1f}%  露出 {r.avg_exp:.2f}")
    for _, r in df.sort_values('oos_cagr').head(2).iterrows():
        print(f"    (下位) {r.label:20s} IS {r.is_cagr:6.2f}% DD {r.is_dd:5.1f}% | OOS {r.oos_cagr:6.2f}% DD {r.oos_dd:5.1f}%  露出 {r.avg_exp:.2f}")
    df.to_csv(f'{OUT}/c_grid_{name.replace("(","").replace(")","").replace("^","")}.csv', index=False)

pd.DataFrame(rows).to_csv(f'{OUT}/c_voltarget.csv', index=False)
print('\nsaved', f'{OUT}/c_voltarget.csv')
