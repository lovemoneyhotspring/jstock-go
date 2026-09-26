"""（事後の点検・採否に使わない）JPX日経中小型株指数の採用の超過（+185 bp、t 7.03）に偏りが無いかを確かめる。

根拠: vault 20-research/2026-09-jp-jsm-rebalance.md の「事後の点検」の続き。
「採用されたから上げた」のか「採用される種類の銘柄（業績が良く、上げてきた小型株）だから上げた」のかを切り分ける。

  bash test/heavy.sh test/.venv/bin/python test/jsm_add_bias.py > test/out/jsm_add_bias.txt 2>&1

■ 比較の母集団（回ごと）
  普通株のうち、どの入替（JPX日経400・中小型の採用・除外）にも関係しない銘柄と、中小型の採用（400 と重ならない）。
  時価総額が採用の 5〜95% の範囲、売買代金 20 日中央値が採用の 5% 点以上に絞る。
■ 回ごとの回帰  超過 ~ 採用 + 共変量。「採用」の係数を回ごとに取り、9 回の平均と t を出す。
  M0: 共変量なし（規模で絞っただけ）  M1: + log 時価総額・log 売買代金
  M2: + 直近の騰落（20・60 日、250 日（直近 20 日を除く））  M3: + ROE（直近の本決算）・営業黒字
  M4: + 窓の中の決算発表の有無  M4x: M4 で窓の中に決算がある銘柄を両方から除く
■ 窓
  本体: 発表の翌寄り → 組み入れの日の引け（jsm_rebalance.py の D1 と同じ）
  遅い入口: 発表の翌営業日の引け → 組み入れの日の引け（発表が場中だった場合の漏れを除く）
  発表前（プラセボ）: 発表の 25 営業日前の引け → 5 営業日前の引け（まだ誰も知らない）
  1 年前・1 年後（プラセボ）: 同じ銘柄・同じ暦の窓を 245 営業日前・後にずらす（共変量もその時点で作り直す）
  組み入れ後: 組み入れの日の引け → 20 営業日後の引け（反落して返すか）
■ 報告
  採用の TOPIX 超過の手取り（流動性別コスト＋滑り 10 bp を引く）、上位・下位 3 銘柄を除いた平均、年ごとの値。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from dt_wf_target import liq_cost_bp  # noqa: E402
from jsm_rebalance import EVENTS  # noqa: E402
from j400_rebalance import EVENTS as J400  # noqa: E402

SLIP_BP = 10.0


def ols_coef(y, X):
    X = np.column_stack([np.ones(len(y)), X])
    b, *_ = np.linalg.lstsq(X, y, rcond=None)
    return b[1]


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va', 'MktCap']}
    O, C = W['AdjO'], W['AdjC']
    va = W['Va'].rolling(20, min_periods=10).median()
    ms = c.execute("SELECT Date, Code, ProdCat FROM master").df()
    ms['Date'] = pd.to_datetime(ms.Date)
    fs = c.execute("""SELECT DiscDate, Code, CurPerType, TRY_CAST(NP AS DOUBLE) NP, TRY_CAST(Eq AS DOUBLE) Eq,
                             TRY_CAST(OP AS DOUBLE) OP FROM fins""").df()
    fs['DiscDate'] = pd.to_datetime(fs.DiscDate)
    fy = fs[fs.CurPerType == 'FY'].dropna(subset=['NP', 'Eq']).sort_values('DiscDate')
    disc = fs[['DiscDate', 'Code']].drop_duplicates()

    def feats(a0, a, b, codes):
        """a0 の時点の共変量と、窓の中の決算の有無。"""
        d = pd.DataFrame(index=codes)
        d['lmc'] = np.log(W['MktCap'].iloc[a0].reindex(codes))
        d['lva'] = np.log(va.iloc[a0].reindex(codes))
        d['m20'] = C.iloc[a0].reindex(codes) / C.iloc[a0 - 20].reindex(codes) - 1
        d['m60'] = C.iloc[a0].reindex(codes) / C.iloc[a0 - 60].reindex(codes) - 1
        d['m250'] = (C.iloc[a0 - 20].reindex(codes) / C.iloc[a0 - 250].reindex(codes) - 1) if a0 >= 250 else np.nan
        f = fy[fy.DiscDate <= tdays[a0]].groupby('Code').last()
        d['roe'] = (f.NP / f.Eq).reindex(codes).clip(-1, 1)
        d['opp'] = (f.OP.reindex(codes) > 0).astype(float)
        w = disc[(disc.DiscDate > tdays[a0]) & (disc.DiscDate <= tdays[b])]
        d['earn'] = d.index.isin(w.Code).astype(float)
        return d

    def cross(a0, s, e, adds, touched, use_open=True):
        """窓 s → e の超過と共変量の表（採用 + 規模の揃った対照）。"""
        if s < 1 or e >= len(tdays) or a0 < 250 - 20:
            return None
        start = O.iloc[s] if use_open else C.iloc[s]
        tstart = tp.open.iloc[s] if use_open else tp.close.iloc[s]
        ex = (C.iloc[e] / start - 1) - (tp.close.iloc[e] / tstart - 1)
        mc, v = W['MktCap'].iloc[a0], va.iloc[a0]
        A = [k for k in adds if k in ex.index and np.isfinite(ex.get(k, np.nan))]
        if len(A) < 10:
            return None
        lo, hi, vlo = mc[A].quantile(.05), mc[A].quantile(.95), v[A].quantile(.05)
        common = set(ms[(ms.Date == tdays[a0]) & (ms.ProdCat == '011')].Code) if (ms.Date == tdays[a0]).any() else set(ex.index)
        ctl = ex.index[ex.notna() & mc.between(lo, hi) & (v >= vlo) & ~ex.index.isin(touched) & ex.index.isin(common)]
        codes = list(A) + list(ctl)
        d = feats(a0, s, e, codes)
        d['ex'] = ex.reindex(codes)
        d['add'] = d.index.isin(A).astype(float)
        return d

    MODELS = {
        'M0': [], 'M1': ['lmc', 'lva'], 'M2': ['lmc', 'lva', 'm20', 'm60', 'm250'],
        'M3': ['lmc', 'lva', 'm20', 'm60', 'm250', 'roe', 'opp'],
        'M4': ['lmc', 'lva', 'm20', 'm60', 'm250', 'roe', 'opp', 'earn'],
    }

    def fit(d, cols, drop_earn=False):
        if drop_earn:
            d = d[d.earn == 0]
        d = d.dropna(subset=['ex', 'add'] + cols)
        if d['add'].sum() < 5:
            return np.nan
        return ols_coef(d.ex.values, d[['add'] + cols].values) * 1e4

    windows = {
        '本体（翌寄り→組み入れ）': lambda a0, a, b: (a, b, True),
        '遅い入口（翌営業日の引け→組み入れ）': lambda a0, a, b: (a, b, False),
        '発表前（−25→−5、プラセボ）': lambda a0, a, b: (a0 - 25, a0 - 5, False),
        '組み入れ後 20 日': lambda a0, a, b: (b, b + 20, False),
        '1 年前の同じ窓（プラセボ）': lambda a0, a, b: (a - 245, b - 245, True),
        '1 年後の同じ窓（プラセボ）': lambda a0, a, b: (a + 245, b + 245, True),
    }
    pd.set_option('display.width', 220)
    main_rows = []
    for wname, wf in windows.items():
        res = []
        for ann, eff, adds, dels in EVENTS:
            a0 = int(np.searchsorted(tdays, pd.Timestamp(ann), side='right')) - 1
            a = a0 + 1
            b = int(np.searchsorted(tdays, pd.Timestamp(eff), side='left')) - 1
            j = [x for x in J400 if x[0] == ann][0]
            touched = {k + '0' for k in (adds + dels + j[2] + j[3]).split()}
            pure = [k + '0' for k in adds.split() if k not in set(j[2].split()) | set(j[3].split())]
            s, e, use_open = wf(a0, a, b)
            base = s - 1 if 'プラセボ' in wname and '年' in wname else a0   # 年をずらす窓は共変量もその時点で
            d = cross(base, s, e, pure, touched, use_open)
            if d is None:
                continue
            r = {'ann': ann, 'n_add': int(d['add'].sum()), 'n_ctl': int((d['add'] == 0).sum()),
                 'earn_add': d.loc[d['add'] == 1, 'earn'].mean(), 'earn_ctl': d.loc[d['add'] == 0, 'earn'].mean()}
            for m, cols in MODELS.items():
                r[m] = fit(d, cols)
            r['M4x'] = fit(d, MODELS['M4'], drop_earn=True)
            res.append(r)
            if wname.startswith('本体'):
                main_rows.append((ann, d, a0, a, b, pure))
        r = pd.DataFrame(res).set_index('ann')
        print(f'\n=== {wname}（採用の係数、bp）')
        print(r.round(2).to_string())
        summ = {}
        for m in list(MODELS) + ['M4x']:
            x = r[m].dropna()
            summ[m] = f'{x.mean():+.1f}（t {x.mean() / x.std(ddof=1) * np.sqrt(len(x)):+.2f}、正 {(x > 0).sum()}/{len(x)}）'
        for k, v in summ.items():
            print(f'  {k}: {v}')
        if len(r):
            x = r['M4']
            isx, oos = x[pd.to_datetime(x.index) <= IS_END], x[pd.to_datetime(x.index) > IS_END]
            print(f'  M4 の IS {isx.mean():+.1f} / OOS {oos.mean():+.1f}')

    # 手取り（TOPIX 超過から流動性別コストを引く）と外れ値
    print('\n=== 採用を等金額で買う手取り（本体の窓、TOPIX 超過 − 流動性別コスト − 滑り 10 bp）')
    out = []
    for ann, d, a0, a, b, pure in main_rows:
        A = d[d['add'] == 1]
        cost = np.array([float(liq_cost_bp(np.nan_to_num(va[k].values[a0]))) for k in A.index]) + SLIP_BP
        net = A.ex.values * 1e4 - cost
        srt = np.sort(net)
        out.append(dict(ann=ann, n=len(A), gross=A.ex.mean() * 1e4, cost=cost.mean(), net=net.mean(),
                        med=np.median(net), trim3=srt[3:-3].mean(), win=(net > 0).mean()))
    o = pd.DataFrame(out).set_index('ann')
    print(o.round(1).to_string())
    x = o.net
    print(f'手取りの回の平均 {x.mean():+.1f} bp  t {x.mean() / x.std(ddof=1) * np.sqrt(len(x)):+.2f}  正 {(x > 0).sum()}/{len(x)}  '
          f'上下 3 銘柄を除く {o.trim3.mean():+.1f}  中央値の平均 {o.med.mean():+.1f}')


if __name__ == '__main__':
    main()
