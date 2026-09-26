"""新興市場の貸借銘柄を月ごとに売る（TOPIX で中立）の汎化: 通年・季節で休む月を walk-forward で決める形・11 月だけ休む形。

根拠: vault 20-research/2026-09-jp-growth-short.md（親: 2026-09-jp-dec-short の D2。12 月 +362 bp/月・8/10 年だが、
月別の平均は結果として見てしまっている: 5・6・8・11 月が負）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/growth_short.py > test/out/growth_short.txt 2>&1

■ 形（dec_short.py の D2 と同じ）
  月の最初の寄りで、前月末の master で新興市場（マザーズ 0104・JASDAQ グロース 0107・グロース 0113）かつ貸借銘柄、
  建てる日の前日までの売買代金 20 日中央値 ≥ 1 億円の銘柄を等金額で売り、TOPIX を同額買う。月の最終営業日の引けで返す。
  純損益 = −（銘柄 − TOPIX）− （流動性別コスト＋滑り 10 bp）− 年 3.1% × 保有営業日/245。
  対象は 2016-09〜2026-08 の完結した月（120 か月）。
■ 形 A（通年）: 毎月売る。
■ 形 B（walk-forward の季節）: y 年の m 月は、y 年より前の m 月の純損益の平均が負なら休む（3 年分たまるまでは休まない）。
  休む月の損益は 0。
■ 形 C（11 月だけ休む）: 結果を見てから決めた形。報告だけで採否に使わない。
■ 指標: 月の純損益の平均・t（月の t）、年率 Sharpe（月次 × √12）、累積（単利）の最大 DD、IS（〜2022-12）/ OOS（2023-01〜）。
  年ごとの合計。
■ 採否
  A（戦略として成り立つか）:
    A1: 月の平均 > 0 かつ t ≥ 2.0
    A2: IS・OOS とも平均 > 0
    A3: Sharpe ≥ 0.5
    A4: 年ごとの合計が、完結した 9 年（2017〜2025。2016・2026 の端は除く）中 6 年以上で正
  B（季節で休む規則が効くか。A を満たすときだけ意味を持つ）:
    B1: Sharpe が A より高い
    B2: OOS の平均が A 以上
  A1〜A4 を満たせば「通年の新興株ショート」を採用候補とする。B1・B2 も満たせば、季節で休む規則（walk-forward の形）を付ける。
  C は B の結果とともに読むだけ（11 月を休むのが良いかは、B で 11 月が休みに選ばれる年の数で判断する）。
■ 報告だけ
  11 月は特異点か: 年ごとの「11 月 − 同じ年の他の月の平均」の t。月別の平均（IS / OOS 別）。
  B が各年に休んだ月。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403

from dt_wf_target import liq_cost_bp  # noqa: E402

MIN_VA = 1e8
BORROW = 0.031
GROWTH = {'0104', '0107', '0113'}
START, END = pd.Timestamp('2016-09-01'), pd.Timestamp('2026-08-31')


def monthly():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va']}
    del px
    codes = W['AdjO'].columns
    O, C = W['AdjO'].values, W['AdjC'].values
    va = W['Va'].rolling(20, min_periods=10).median().shift(1).values
    ms = c.execute("SELECT Date, Code, Mrgn, Mkt FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
    ms['Date'] = pd.to_datetime(ms.Date)
    snaps = {d: g for d, g in ms.groupby('Date')}
    sdays = sorted(snaps)
    rows = []
    for p in pd.period_range(START, END, freq='M'):
        md = np.nonzero((tdays.year == p.year) & (tdays.month == p.month))[0]
        if len(md) < 10:
            continue
        md = md[np.isfinite(O[md]).any(axis=1)]  # 終日止まった日（2020-10-01）は個別株の値が無い
        a, b = md[0], md[-1]
        prior = [d for d in sdays if d < tdays[a]]
        g = snaps[prior[-1]]
        sel = codes.isin(g.Code[(g.Mrgn.astype(str) == '2') & g.Mkt.isin(GROWTH)]) & (np.nan_to_num(va[a]) >= MIN_VA)
        r = C[b] / O[a] - 1 - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
        cst = (liq_cost_bp(np.nan_to_num(va[a])) + 10.0) / 1e4 + BORROW * (b - a + 1) / 245
        net = (-r - cst)[sel & np.isfinite(r)]
        rows.append(dict(ym=p, y=p.year, m=p.month, net=net.mean() * 1e4, n=len(net)))
    return pd.DataFrame(rows).set_index('ym')


def stat(x):
    x = x.dropna()
    eq = x.cumsum()
    return dict(mean=x.mean(), t=x.mean() / x.std(ddof=1) * np.sqrt(len(x)), sharpe=x.mean() / x.std(ddof=1) * np.sqrt(12),
                dd=(eq - eq.cummax()).min())


def main():
    df = monthly()
    df.to_csv(f'{OUT}/growth_short.csv')
    A = df.net
    # B: walk-forward の季節
    skip = []
    for ym, r in df.iterrows():
        past = df[(df.y < r.y) & (df.m == r.m)].net
        skip.append(len(past) >= 3 and past.mean() < 0)
    df['skipB'] = skip
    B = df.net.where(~df.skipB, 0.0)
    Cc = df.net.where(df.m != 11, 0.0)
    ise = df.index.to_timestamp() <= IS_END

    def show(name, x):
        s, si, so = stat(x), stat(x[ise]), stat(x[~ise])
        yr = x.groupby(df.y).sum()
        print(f'{name}: 平均 {s["mean"]:+.1f} bp/月  t {s["t"]:+.2f}  Sharpe {s["sharpe"]:.2f}  DD {s["dd"]:+.0f} bp  '
              f'IS {si["mean"]:+.1f}  OOS {so["mean"]:+.1f}')
        print('   年: ' + ' '.join(f'{y}:{v:+.0f}' for y, v in yr.items()))
        return s, si, so, yr

    print(f'月数 {len(df)}  1 か月の銘柄数 平均 {df.n.mean():.0f}（最少 {df.n.min()}）\n')
    sa, sai, sao, yra = show('A 通年', A)
    sb, sbi, sbo, _ = show('B walk-forward', B)
    show('C 11 月だけ休む（報告だけ）', Cc)
    full = yra[(yra.index >= 2017) & (yra.index <= 2025)]
    a1 = sa['mean'] > 0 and sa['t'] >= 2.0
    a2 = sai['mean'] > 0 and sao['mean'] > 0
    a3 = sa['sharpe'] >= 0.5
    a4 = int((full > 0).sum()) >= 6
    b1 = sb['sharpe'] > sa['sharpe']
    b2 = sbo['mean'] >= sao['mean']
    print(f'\n判定 A: 1 {a1}  2 {a2}  3 {a3}  4 {a4}（正の年 {int((full > 0).sum())}/{len(full)}）'
          f' → {"採用候補" if a1 and a2 and a3 and a4 else "不採用"}')
    print(f'判定 B: 1 {b1}  2 {b2} → {"季節で休む規則を付ける" if a1 and a2 and a3 and a4 and b1 and b2 else "付けない"}')

    print('\n--- 報告 ---')
    mon = pd.DataFrame({'全': df.groupby('m').net.mean(), 'IS': df[ise].groupby('m').net.mean(),
                        'OOS': df[~ise].groupby('m').net.mean(), '正の年': df.groupby('m').net.apply(lambda s: f'{(s > 0).sum()}/{len(s)}')})
    print(mon.round(1).T.to_string())
    oth = df[df.m != 11].groupby('y').net.mean()
    nov = df[df.m == 11].set_index('y').net
    d = (nov - oth).dropna()
    print(f'\n11 月 − 同じ年の他の月: 平均 {d.mean():+.1f} bp  t {d.mean() / d.std(ddof=1) * np.sqrt(len(d)):+.2f}  （{len(d)} 年）')
    sk = df[df.skipB].groupby('y').m.apply(list)
    print('B が休んだ月:', ' '.join(f'{y}:{v}' for y, v in sk.items()))
    print('B が 11 月を休んだ年:', int(df[(df.m == 11) & df.skipB].shape[0]), '/', int((df.m == 11).sum()))


if __name__ == '__main__':
    main()
