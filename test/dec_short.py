"""12 月にだけ強いショートを探す（順張り以外の仕組み）。候補 5 本を同じ基準で測る。

根拠: vault 20-research/2026-09-jp-dec-short.md（親: 2026-09-jp-dec-taxloss。ユーザの問い「12 月にだけ強いショート戦法」）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/dec_short.py > test/out/dec_short.txt 2>&1

■ 候補（仕組み）
  D1 寄り後の続落（daytrade が 12 月を休む理由。日中の順張りに近いので対照として入れる）:
     ギャップ（前日終値→始値、調整済み）が −15%〜−2% の銘柄を寄りで売り、引けで買い戻す。
  D3 寄りの跳ねの逆張り: ギャップが +3%〜+15% の銘柄を寄りで売り、引けで買い戻す。
     D1・D3 は母集団 = 内国株かつ貸借銘柄（月末の master）で前日の売買代金 20 日中央値 ≥ 1 億円。
     前日か当日に決算発表（fins_earnings_date の SchDate）がある銘柄は除く。1 日 3 銘柄以上の日だけ数える。
  D2 IPO ラッシュの資金の吸い上げ（新興市場）: 月の最初の寄りで、新興市場（マザーズ・JASDAQ グロース・グロース）の
     貸借銘柄で売買代金 20 日中央値 ≥ 1 億円を等金額で売り、月の最終営業日の引けで買い戻す。TOPIX に対する超過。
  D2b 同じ窓で、上場から 1 年以内の銘柄（bars に最初に現れた日が月初の 365 日前以降、かつ 2016-09 以降）。
     貸借は問わない（一般信用で売れる前提。実際に売れるかは別途）。売買代金の下限は同じ。
  D7 その月の IPO の上場直後: 前月 21 日〜当月 20 日に bars に最初に現れた銘柄を、2 営業日目の寄りで売り、
     10 営業日目の引けで買い戻す。TOPIX に対する超過。
■ 費用
  日中（D1・D3）: dt_wf_target.liq_cost_bp（前日の売買代金 20 日中央値）＋滑り 5 bp。
  持ち越し（D2・D2b・D7）: liq_cost_bp ＋滑り 10 bp ＋貸株料と逆日歩の見込み 年 3.1% × 保有営業日/245。
■ 年の値
  D1・D3: その月の日ごとの等金額平均（bp/日）を月内で平均。D2・D2b・D7: 月の窓の等金額平均（bp/窓）。
  12 月は 2016〜2025 の 10 回。対照 = 同じ年の 1〜11 月（D7 は IPO のある月だけ）。
■ 採否（候補ごと。5 本なので t の基準を上げる）
  1: 12 月のショートの純損益が 10 年中 7 年以上で正
  2: 12 月の純損益の平均 > 0 かつ t ≥ 2.3（D1・D3 は 12 月の全日を 1 標本とする日の t、D2・D2b・D7 は年の t）
  3: 年ごとの「12 月 − 同じ年の対照の平均」が正で、その年の t ≥ 2.0（12 月に特有）
  すべて満たせば採用候補（12 月だけの戦術として、資金・件数・約定を別に事前登録）。満たさなければ不採用。
■ 報告だけ
  月別（1〜12 月）の平均、年ごとの値、件数。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403

from dt_wf_target import liq_cost_bp  # noqa: E402

MIN_VA = 1e8
BORROW = 0.031
YEARS = range(2016, 2026)
GROWTH = {'0104', '0107', '0113'}


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    first = px.groupby('Code').Date.min()
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va']}
    del px
    codes = W['AdjO'].columns
    O, C = W['AdjO'].values, W['AdjC'].values
    va = W['Va'].rolling(20, min_periods=10).median().shift(1).values   # 前日までの中央値
    T, N = O.shape

    # 月末の master → 翌月: 貸借・新興
    ms = c.execute("SELECT Date, Code, Mrgn, Mkt FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
    ms['Date'] = pd.to_datetime(ms.Date)
    snaps = {d: g for d, g in ms.groupby('Date')}
    sdays = sorted(snaps)
    mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
    lend = np.zeros((T, N), bool)
    grow = np.zeros((T, N), bool)
    dom = np.zeros((T, N), bool)
    for k in range(len(mends) - 1):
        me, nx = mends.iloc[k], mends.iloc[k + 1]
        prior = [d for d in sdays if d <= me]
        if not prior:
            continue
        g = snaps[prior[-1]]
        rows = (tdays > me) & (tdays <= nx)
        dom[rows] = codes.isin(g.Code)
        lend[rows] = codes.isin(g.Code[g.Mrgn.astype(str) == '2'])
        grow[rows] = codes.isin(g.Code[g.Mkt.isin(GROWTH)])

    ed = c.execute(f"SELECT Code, SchDate FROM read_parquet('{ROOT}/fins_earnings_date/*.parquet', union_by_name=true)").df()
    sd = pd.to_datetime(ed.SchDate)
    ed['SchDate'] = sd.dt.tz_localize(None) if sd.dt.tz is not None else sd
    earn = np.zeros((T, N), bool)
    ci = codes.get_indexer(ed.Code)
    ti = np.searchsorted(tdays, ed.SchDate.values)
    ok = (ci >= 0) & (ti < T) & (tdays[np.minimum(ti, T - 1)] == ed.SchDate.values)
    earn[ti[ok], ci[ok]] = True
    earn_near = earn | np.vstack([np.zeros((1, N), bool), earn[:-1]])   # 当日か前日

    prevC = np.vstack([np.full((1, N), np.nan), C[:-1]])
    gap = O / prevC - 1
    intr = C / O - 1
    cost_d = (liq_cost_bp(np.nan_to_num(va)) + 5.0) / 1e4
    base = dom & lend & (va >= MIN_VA) & ~earn_near & np.isfinite(gap) & np.isfinite(intr)

    rows = []
    # D1・D3（日中）
    for name, lo, hi in (('D1', -0.15, -0.02), ('D3', 0.03, 0.15)):
        m = base & (gap >= lo) & (gap <= hi)
        net = np.where(m, -intr - cost_d, np.nan)
        cnt = m.sum(axis=1)
        day = np.where(cnt >= 3, np.nanmean(net, axis=1), np.nan)
        s = pd.Series(day, index=tdays).dropna()
        for (y, mo), v in s.groupby([s.index.year, s.index.month]):
            rows.append(dict(name=name, y=y, m=mo, v=v.mean() * 1e4, n=len(v)))
        dec = s[s.index.month == 12]
        rows.append(dict(name=name + '_days', y=0, m=12, v=dec.mean() / dec.std() * np.sqrt(len(dec)), n=len(dec)))

    # D2・D2b（月の窓）
    fpos = pd.Series(np.searchsorted(tdays, first.reindex(codes).values), index=codes)
    for y in YEARS:
        for mo in range(1, 13):
            md = np.nonzero((tdays.year == y) & (tdays.month == mo))[0]
            if len(md) < 10 or md[0] == 0:
                continue
            a, b = md[0], md[-1]
            r = C[b] / O[a] - 1 - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
            cst = (liq_cost_bp(np.nan_to_num(va[a])) + 10.0) / 1e4 + BORROW * (b - a + 1) / 245
            netw = -r - cst
            liq = dom[a] & (va[a] >= MIN_VA) & np.isfinite(r)
            g2 = liq & grow[a] & lend[a]
            new = (first.reindex(codes).values >= tdays[a] - pd.Timedelta(days=365)) & \
                  (first.reindex(codes).values >= pd.Timestamp('2016-09-01'))
            g2b = liq & new
            for name, g in (('D2', g2), ('D2b', g2b)):
                if g.sum() >= 3:
                    rows.append(dict(name=name, y=y, m=mo, v=np.nanmean(netw[g]) * 1e4, n=int(g.sum())))
            # D7: 前月 21 日〜当月 20 日に上場
            lo_d = (pd.Timestamp(y, mo, 1) - pd.DateOffset(months=1)).replace(day=21)
            hi_d = pd.Timestamp(y, mo, 20)
            ipo = [k for k, d in first.items() if lo_d <= d <= hi_d and d >= pd.Timestamp('2016-09-01')]
            vals = []
            for k in ipo:
                j = codes.get_loc(k)
                p0 = int(fpos[k])
                e, x = p0 + 1, p0 + 9
                if x >= T or not (np.isfinite(O[e, j]) and np.isfinite(C[x, j])):
                    continue
                rr = C[x, j] / O[e, j] - 1 - (tp.close.iloc[x] / tp.open.iloc[e] - 1)
                cc = (float(liq_cost_bp(np.nan_to_num(va[e, j]))) + 10.0) / 1e4 + BORROW * 9 / 245
                vals.append(-rr - cc)
            if len(vals) >= 2:
                rows.append(dict(name='D7', y=y, m=mo, v=np.mean(vals) * 1e4, n=len(vals)))

    df = pd.DataFrame(rows)
    df.to_csv(f'{OUT}/dec_short.csv', index=False)
    pd.set_option('display.width', 220)

    def tt(x):
        x = np.asarray(x, float)
        x = x[np.isfinite(x)]
        return x.mean() / x.std(ddof=1) * np.sqrt(len(x)) if len(x) > 2 else np.nan

    for name in ('D1', 'D3', 'D2', 'D2b', 'D7'):
        d = df[df.name == name]
        dec = d[d.m == 12].set_index('y').v
        plc = d[d.m != 12].groupby('y').v.mean()
        diff = (dec - plc).dropna()
        mon = d.groupby('m').v.mean()
        t2 = df[(df.name == name + '_days')].v.iloc[0] if name in ('D1', 'D3') else tt(dec)
        c1 = int((dec > 0).sum()) >= 7
        c2 = dec.mean() > 0 and t2 >= 2.3
        c3 = diff.mean() > 0 and tt(diff) >= 2.0
        print(f'\n=== {name}（純損益 bp、{"1 日あたり" if name in ("D1", "D3") else "1 窓あたり"}）')
        print('12 月:', ' '.join(f'{y}:{v:+.0f}' for y, v in dec.items()), f' 件数/月 {d[d.m == 12].n.mean():.0f}')
        print('月別:', ' '.join(f'{m}月 {v:+.1f}' for m, v in mon.items()))
        print(f'12 月 平均 {dec.mean():+.1f}  t {t2:+.2f}  正 {int((dec > 0).sum())}/{len(dec)}   '
              f'12 月 − 対照 {diff.mean():+.1f}（t {tt(diff):+.2f}）')
        print(f'判定 {name}: 1 {c1}  2 {c2}  3 {c3}  → {"採用候補" if c1 and c2 and c3 else "不採用"}')


if __name__ == '__main__':
    main()
