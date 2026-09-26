"""日経平均の定期入替: 発表から実施前日までの需給（W1）と、実施後の反転（W2）を取れるか。

根拠: vault 20-research/2026-09-jp-n225-rebalance.md
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/n225_rebalance.py > test/out/n225_rebalance.txt 2>&1

■ 事象（下の EVENTS。出典は日経の「日経平均株価銘柄変更履歴（2026/4/1 現在）」の表と、各回の発表資料
  indexes.nikkei.co.jp/nkave/archives/news/<発表日>J_1.pdf。2017 年の発表日は日経の記事で確認）
  定期見直しによる入替だけ（2016-10〜2026-04 の 14 回、採用 28・除外 27）。合併・上場廃止に伴う臨時の入替は入れない。
  2022-09-29 の日本電産は定期見直しでの採用だが、静岡銀行の上場廃止に合わせた実施日なので、その日を実施日とする。
  2026-10-01（2026-09-04 発表）は前向きの標本として別に出し、採否に使わない。
■ 窓（発表は引け後とみなす）
  W1: 発表日の翌営業日の寄りで、採用を買い・除外を売る → 実施日の前営業日（指数の組み入れの日）の引けで返す。
  W2: 実施日の前営業日の引けで、採用を売り・除外を買う → その 10 営業日後の引けで返す。
■ 指標
  超過 = 銘柄の窓のリターン − TOPIX の同じ窓のリターン（寄り・引けも合わせる）。
  純損益 = 向き × 超過 − （dt_wf_target.liq_cost_bp（発表日の売買代金 20 日中央値）＋滑り 10 bp）
          − 売る脚だけ 年 3.1% × 保有営業日/245。
  1 回（実施日）ごとに採用と除外の全銘柄の純損益を等金額で平均し、その回の値とする（14 標本）。
■ 採否（W1・W2 それぞれ）
  1: IS（実施日 〜2022-12）・OOS（2023-01〜）とも回の平均 > 0
  2: 14 回の t ≥ 2.0
  3: 14 回中 10 回以上で正
  すべて満たせば採用候補（2026-10-01 以降の回を前向きに紙の上で追い、実運用の形を別に事前登録する）。
■ 報告だけ
  採用・除外の脚ごとの表、売る脚が貸借銘柄でない件数、2026-10-01 の回（W2 は 9/30 の引けで建てるので、
  データが入り次第）。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403

from dt_wf_target import liq_cost_bp  # noqa: E402

SLIP_BP = 10.0
BORROW = 0.031
W2_DAYS = 10

# (発表日, 実施日, 採用, 除外)
EVENTS = [
    ('2016-09-06', '2016-10-03', ['4755'], ['4041']),
    ('2017-09-05', '2017-10-02', ['6098', '6178'], ['3865', '6508']),
    ('2018-09-05', '2018-10-01', ['4751'], ['5715']),
    ('2019-09-04', '2019-10-01', ['2413'], ['9681']),
    ('2020-09-01', '2020-10-01', ['9434'], ['4272']),
    ('2021-09-06', '2021-10-01', ['6861', '6981', '7974'], ['3105', '5901', '9412']),
    ('2022-09-05', '2022-09-29', ['6594'], []),
    ('2022-09-05', '2022-10-03', ['6273', '7741'], ['3103', '6703']),
    ('2023-03-03', '2023-04-03', ['4661', '6723', '9201'], ['3101', '5703', '5707']),
    ('2023-09-04', '2023-10-02', ['4385', '6920', '9843'], ['5202', '7003', '8628']),
    ('2024-03-04', '2024-04-01', ['3092', '6146', '6526'], ['2531', '5232', '5541']),
    ('2024-09-04', '2024-10-01', ['4307', '7453'], ['3863', '4631']),
    ('2025-03-05', '2025-04-01', ['6532'], ['9301']),
    ('2025-09-08', '2025-10-01', ['3697'], ['7762']),
    ('2026-03-05', '2026-04-01', ['285A', '543A', '7532'], ['6674', '6952', '7205']),
]
FORWARD = [('2026-09-04', '2026-10-01', ['5016', '6525', '9697'], ['4902', '543A', '7004'])]
# 2022-09-29 と 10-03 は同じ発表の 1 回として数える
ROUND = {'2022-09-29': '2022-10-03'}


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va']}
    va = W['Va'].rolling(20, min_periods=10).median()
    mg = c.execute("SELECT Date, Code, Mrgn FROM master").df()
    mg['Date'] = pd.to_datetime(mg.Date)

    def pos_after(d):
        return int(np.searchsorted(tdays, pd.Timestamp(d), side='right'))

    def pos_before(d):
        return int(np.searchsorted(tdays, pd.Timestamp(d), side='left')) - 1

    def shortable(code, d):
        s = mg[(mg.Code == code) & (mg.Date <= pd.Timestamp(d))]
        return bool(len(s)) and str(s.sort_values('Date').Mrgn.iloc[-1]) == '2'

    rows = []
    for ann, eff, adds, dels in EVENTS + FORWARD:
        a = pos_after(ann)          # 発表の翌営業日
        b = pos_before(eff)         # 実施日の前営業日（組み入れの日）
        if pd.Timestamp(eff) > tdays[-1]:
            b = len(tdays)          # 組み入れの日がまだデータに無い
        z = b + W2_DAYS
        for side, codes in ((1, adds), (-1, dels)):
            for k in codes:
                code = k + '0'
                if code not in W['AdjC'].columns:
                    rows.append(dict(eff=eff, code=k, side=side, note='データ無し'))
                    continue
                O, C = W['AdjO'][code].values, W['AdjC'][code].values
                cost = (float(liq_cost_bp(np.nan_to_num(va[code].values[a - 1]))) + SLIP_BP) / 1e4
                r = dict(eff=ROUND.get(eff, eff), code=k, side=side, fwd=(ann, eff, adds, dels) in FORWARD,
                         shortable=shortable(code, ann))
                if b < len(tdays) and np.isfinite(O[a]) and np.isfinite(C[b]):
                    ex1 = (C[b] / O[a] - 1) - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
                    r['w1_ex'] = ex1
                    r['w1'] = side * ex1 - cost - (BORROW * (b - a + 1) / 245 if side < 0 else 0)
                if z < len(tdays) and np.isfinite(C[b]) and np.isfinite(C[z]):
                    ex2 = (C[z] / C[b] - 1) - (tp.close.iloc[z] / tp.close.iloc[b] - 1)
                    r['w2_ex'] = ex2
                    r['w2'] = -side * ex2 - cost - (BORROW * W2_DAYS / 245 if side > 0 else 0)
                rows.append(r)
    df = pd.DataFrame(rows)
    df.to_csv(f'{OUT}/n225_rebalance.csv', index=False)
    print(df[df.get('note').notna()] if 'note' in df else '', flush=True)
    df = df[df.get('note', pd.Series(index=df.index, dtype=object)).isna()]
    hist, fwd = df[~df.fwd.astype(bool)], df[df.fwd.astype(bool)]

    pd.set_option('display.width', 200)
    for w in ('w1', 'w2'):
        g = hist.dropna(subset=[w]).groupby('eff')
        per = pd.DataFrame({'n': g.size(), 'net_bp': g[w].mean() * 1e4,
                            'add_ex_bp': g.apply(lambda x: x.loc[x.side > 0, f'{w}_ex'].mean() * 1e4),
                            'del_ex_bp': g.apply(lambda x: x.loc[x.side < 0, f'{w}_ex'].mean() * 1e4)})
        x = per.net_bp
        isx, oos = x[pd.to_datetime(x.index) <= IS_END], x[pd.to_datetime(x.index) > IS_END]
        t = x.mean() / x.std(ddof=1) * np.sqrt(len(x))
        c1, c2, c3 = isx.mean() > 0 and oos.mean() > 0, t >= 2.0, int((x > 0).sum()) >= 10
        print(f'\n=== {w.upper()}（{"発表→組み入れの日の引け" if w == "w1" else "組み入れの日の引け→10 営業日後"}）')
        print(per.round(1).to_string())
        print(f'回の平均 {x.mean():+.1f} bp  t {t:+.2f}  正 {int((x > 0).sum())}/{len(x)}  IS {isx.mean():+.1f}  OOS {oos.mean():+.1f}')
        leg = hist.dropna(subset=[w]).groupby('side')[f'{w}_ex'].agg(['mean', 'count'])
        print('脚ごとの超過（bp）:', {('採用' if s > 0 else '除外'): f'{m * 1e4:+.1f} ({n})' for s, (m, n) in leg.iterrows()})
        print(f'判定 {w.upper()}: 1 {c1}  2 {c2}  3 {c3}  → {"採用候補" if c1 and c2 and c3 else "不採用"}')
    ns = hist[(hist.side < 0) & ~hist.shortable.astype(bool)].shape[0]
    print(f'\nW1 で売る除外銘柄のうち貸借でないもの: {ns}/{int((hist.side < 0).sum())}')
    print('\n前向きの回（2026-10-01、採否に使わない）:')
    print(fwd[[k for k in ('code', 'side', 'w1_ex', 'w1', 'w2_ex', 'w2') if k in fwd]].round(4).to_string(index=False))


if __name__ == '__main__':
    main()
