"""制度信用の 6 か月期日の売り: 6 か月前に制度信用の買い残が急に積み上がった銘柄は、期日の前後に超過リターンが負になるか。

根拠: vault 20-research/2026-09-jp-margin-expiry.md
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/margin_expiry.py > test/out/margin_expiry.txt 2>&1

■ 事象（積み上がりの週）
  週次の信用残（markets_margin_interest、Date = 申込みの週の金曜）の制度信用の買い残 LongStdVol。
  spike = (LongStdVol_w − LongStdVol_{w−1}) / その週の最終営業日までの 20 日平均出来高（生の Vo）。
  その週の母集団で spike が上位 10% かつ spike > 0 の銘柄を事象とする。
■ 母集団（その週の時点で作る）
  月末の master で内国株（ProdCat=011、TOKYO PRO 除く、S33≠9999）かつ貸借銘柄（Mrgn=2）を翌月に有効とし、
  積み上がりの週の最終営業日の売買代金 20 日中央値 ≥ 1 億円。
■ 期日の窓
  期日の週の終わり E = (積み上がりの週の金曜 + 6 か月) 以前の最後の営業日、期日の週の始め = E の 4 営業日前。
  建て = 期日の週の始めの 5 営業日前の寄り、返し = E の翌営業日の寄り（約 10 営業日）。
  上場廃止などで返しの寄りが無い銘柄は最後の寄りで返す。
■ 群
  A: 事象すべて
  B（本命）: 含み損 = 建てる前日の終値 < 積み上がりの週の終値の平均（調整済み）。期日まで持って投げる層
■ 指標
  超過 = 銘柄の寄り→寄り − 同じ窓の母集団（建てる日の母集団。事象の有無を問わない）の等金額平均。
  ショートの純損益 = −超過 − 費用（dt_wf_target.liq_cost_bp（売買代金 20 日中央値）＋滑り 10 bp、往復）
                    − 貸株料と逆日歩の見込み 年 3.1% × 保有営業日/245。
  1 つの積み上がりの週の事象は同じ窓を共有するので、週ごとに等金額で平均してから（週を 1 標本に畳んで）t を出す。
■ 期間
  IS = 建てる日が 〜2022-12、OOS = 2023-01〜。
■ 採否（B で判定）
  1: IS・OOS とも純損益の平均 > 0
  2: 全期間の t ≥ 2.5（週に畳んだ t）
  3: OOS の t ≥ 2.0
  4（6 か月に特有か）: 同じ事象で窓を 5 か月・7 か月にずらした対照より、6 か月の超過が低い（両方より負）
  すべて満たせば採用候補。そのときは実運用の形（ショートの籠・daytrade の除外条件・ロングの見送り）を別の事前登録で測る。
  満たさなければ不採用。
■ 報告だけ
  A の同じ表、B の超過（費用前）、期日の前後 −15〜+10 営業日の累積超過の形、事象の件数と週の数。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403

from dt_wf_target import liq_cost_bp  # noqa: E402

TOPQ = 0.10
MIN_VA = 1e8
SLIP_BP = 10.0
BORROW = 0.031
POST = 10


def load():
    c = con()
    tdays = topix(c).index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Vo, Va FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Vo', 'Va']}
    codes = W['AdjO'].columns
    master = c.execute("SELECT Date, Code, Mrgn FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
    master['Date'] = pd.to_datetime(master.Date)
    msnap = {d: set(g.Code[g.Mrgn.astype(str) == '2']) for d, g in master.groupby('Date')}
    mdays = sorted(msnap)
    mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
    elig = pd.DataFrame(False, index=tdays, columns=codes)
    for k in range(len(mends) - 1):
        me, nx = mends.iloc[k], mends.iloc[k + 1]
        prior = [d for d in mdays if d <= me]
        if prior:
            elig.loc[(tdays > me) & (tdays <= nx), elig.columns.isin(msnap[prior[-1]])] = True
    mg = c.execute(f"SELECT Date, Code, TRY_CAST(LongStdVol AS DOUBLE) L FROM read_parquet('{ROOT}/markets_margin_interest/*.parquet', union_by_name=true)").df()
    mg['Date'] = pd.to_datetime(mg.Date).dt.tz_localize(None)
    return tdays, W, elig, mg


def main():
    tdays, W, elig, mg = load()
    O, C = W['AdjO'], W['AdjC']
    Of = O.ffill(limit=30)
    adv = W['Vo'].rolling(20, min_periods=15).mean()
    va = W['Va'].rolling(20, min_periods=15).median()
    liq = elig & (va >= MIN_VA)
    tpos = pd.Series(np.arange(len(tdays)), index=tdays)

    mg = mg.dropna().sort_values(['Code', 'Date'])
    mg['dL'] = mg.groupby('Code').L.diff()
    mg = mg[mg.Code.isin(set(O.columns))]
    # 週の最終営業日（金曜が休みなら前の営業日）
    mg['wend'] = tdays[np.searchsorted(tdays, mg.Date.values, side='right') - 1]
    mg = mg[mg.Date - mg.wend <= pd.Timedelta(days=4)]
    wi = tpos.reindex(mg.wend).values
    ci = O.columns.get_indexer(mg.Code)
    mg['spike'] = mg.dL.values / adv.values[wi, ci]
    mg['ok'] = liq.values[wi, ci]
    mg = mg[mg.ok & np.isfinite(mg.spike)]
    thr = mg.groupby('wend').spike.transform(lambda s: s.quantile(1 - TOPQ))
    ev = mg[(mg.spike >= thr) & (mg.spike > 0)].copy()
    print(f'事象 {len(ev)}  週 {ev.wend.nunique()}', flush=True)

    # 積み上がりの週の終値の平均（含み損の判定用）
    Cm = C.rolling(5, min_periods=3).mean()
    ev['cost_px'] = Cm.values[tpos.reindex(ev.wend).values, O.columns.get_indexer(ev.Code)]

    def window(fri, months):
        e = fri + pd.DateOffset(months=months)
        k = np.searchsorted(tdays, e, side='right') - 1
        ent, ext = k - 4 - 5, k + 1
        if ent < 0 or ext >= len(tdays):
            return None
        return ent, ext, k

    res, curves = {}, []
    for months in (5, 6, 7):
        rows = []
        for (fri, wend), g in ev.groupby(['Date', 'wend']):
            w = window(fri, months)
            if w is None:
                continue
            ent, ext, k = w
            u = liq.values[ent]
            ro = Of.values[ext] / O.values[ent] - 1
            umean = np.nanmean(ro[u & np.isfinite(ro)])
            ci = O.columns.get_indexer(g.Code)
            r = ro[ci]
            ex = r - umean
            loss = C.values[ent - 1, ci] < g.cost_px.values
            cost = (liq_cost_bp(va.values[ent, ci]) + SLIP_BP) / 1e4 + BORROW * (ext - ent) / 245
            net = -ex - cost
            d = pd.DataFrame({'ex': ex, 'net': net, 'B': loss}).dropna()
            for grp, dd in (('A', d), ('B', d[d.B])):
                if len(dd):
                    rows.append((tdays[ent], grp, dd.ex.mean(), dd.net.mean(), len(dd)))
            if months == 6 and ent >= 5 and loss.any():
                s0, s1 = ent - 5, min(k + POST, len(tdays) - 1)
                path = C.values[s0:s1]
                bm = np.nanmean(np.where(liq.values[ent], path, np.nan), axis=1)
                rel = path[:, ci[loss]] / path[0, ci[loss]] - (bm / bm[0])[:, None]
                curves.append(pd.Series(np.nanmean(rel, axis=1), name=fri))
        res[months] = pd.DataFrame(rows, columns=['d', 'grp', 'ex', 'net', 'n']).set_index('d')

    def tt(x):
        x = x.dropna()
        return x.mean() / x.std() * np.sqrt(len(x)) if len(x) > 5 else np.nan

    print('\n月 群  期間   週   件数/週  超過bp  純bp   t')
    out = {}
    for months, r in res.items():
        for grp in ('A', 'B'):
            x = r[r.grp == grp]
            for per, xx in (('IS', x[x.index <= IS_END]), ('OOS', x[x.index > IS_END]), ('全', x)):
                out[(months, grp, per)] = (xx.ex.mean() * 1e4, xx.net.mean() * 1e4, tt(xx.net))
                print(f'{months}  {grp}  {per:>3}  {len(xx):4d}  {xx.n.mean():6.1f}  {xx.ex.mean() * 1e4:+7.1f}  '
                      f'{xx.net.mean() * 1e4:+7.1f}  {tt(xx.net):+.2f}')
    b = lambda p: out[(6, 'B', p)]  # noqa: E731
    c1 = b('IS')[1] > 0 and b('OOS')[1] > 0
    c2 = b('全')[2] >= 2.5
    c3 = b('OOS')[2] >= 2.0
    c4 = b('全')[0] < out[(5, 'B', '全')][0] and b('全')[0] < out[(7, 'B', '全')][0]
    print(f'\n判定（B・6 か月）: 1 {c1}  2 {c2}  3 {c3}  4 {c4}  → {"採用候補" if c1 and c2 and c3 and c4 else "不採用"}')
    if curves:
        cv = pd.concat(curves, axis=1).mean(axis=1) * 1e4
        print('\n累積超過（B、建てる 5 営業日前＝期日の週の始めの 10 営業日前を 0、終値、bp）:')
        print(' '.join(f'{v:+.0f}' for v in cv.values))
    for m, r in res.items():
        r.to_csv(f'{OUT}/margin_expiry_{m}m.csv')


if __name__ == '__main__':
    main()
