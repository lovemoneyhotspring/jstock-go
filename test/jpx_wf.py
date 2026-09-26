"""JPX Kaggle（2022）上位解法の考え方を、日本株の数日〜数週間の銘柄選定として walk-forward で測る。

根拠: vault 20-research/2026-09-jp-jpx-kaggle-wf.md（解法 10 本の調査と、この検証の要点）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/jpx_wf.py > test/out/jpx_wf.txt 2>&1
  test/.venv/bin/python test/jpx_wf.py --prelim   # コードの確認用（H=5・1 シード・2019〜2020 の 2 fold）。採否に使わない

■ 母集団（時点で作る）
  各月末の master（内国株 ProdCat=011、TOKYO PRO 除く、S33≠9999）∩ 時価総額上位 500 を翌月に有効とする（N1 と同じ）。
  ショートの脚は月末の master で貸借銘柄（Mrgn=2）に限る。
■ 建て方
  日 t の引けまでの情報で並べ、t+1 の寄りで建て、t+1+H の寄りで返す。H ∈ {1, 5, 10, 20}。
  毎日 1 籠を作り H 日持つ（籠は H 本が並走する形）。上場廃止などで返しの寄りが無い銘柄は最後の寄りで返す。
  ロング = 上位 20 銘柄の等金額、ショート = 下位 20 銘柄（貸借のみ）の等金額。
■ 形と費用（1 籠の保有期間あたり）
  LS: ロング − ショート − 31.4bp（両脚 × 往復 15.7bp）− 貸株料と逆日歩の見込み 年 3.1% × H/245
  LT: ロング − TOPIX（同じ寄り→寄り）− 15.7bp
■ 特徴量（日ごとに母集団内の百分位。欠損は 0.5。市場平均の 2 本だけ生の値）
  5 位: mr2（2 日リターン）, mom131_25（25 日ずらしの 131 日リターン）, vol231, adv11（売買代金 11 日平均）, price（生の終値）,
        ave_mr2・ave_mom（母集団の平均。生の値）
  2 位: ret20/40/60, vol20/40/60, magap20/40/60（終値と移動平均の乖離）
  4 位・9 位: ret1, intraday（当日の寄り→引け）
  5 位の銘柄コード（カテゴリ）は入れない（銘柄の丸暗記と、生存銘柄での過大評価を避ける）。
■ モデル（LightGBM。設定は test/dt_lgbm_train.KW。末尾 250 日で木の本数を決め、学習期間の全体で学び直す）
  M1: 回帰。目的変数 = 日ごとの市場平均差（アルファ）を正規分位点にしたもの（z）
  M2: M1 を上下 12.5% の端だけで学習（5 位の「上下 250 / 2000」）
  M3: LambdaRank。ラベル = 日ごとのアルファの十分位（0〜9）、グループ = 日、ndcg@20 で early stopping
  シードは 0,1,2。予測は日ごとの百分位にしてから平均する。
  fold は暦年（2019〜2026）。学習はその年の前日まで（2017-07 から）、末尾の H+5 営業日は捨てる。
■ 規則（学習なし、5・4・9 位の本体）
  R1: −mr2 の順（2 日リターンの逆張り）/ R2: −ret1 / R3: −intraday
  R1〜R3 は H=1 で「コンペの形」（t+1 の引けで建て t+2 の引けで返す、費用は同じ）も測る（形の名前に _jpx を付ける）。
■ 期間
  IS = 2019〜2022 の信号日、OOS = 2023-01〜。Sharpe はすべての期間で。
■ 採否（モデル × H × 形の各升。升は 3 モデル × 4 H × 2 形 + 規則 3 × (4 H × 2 形 + 2) = 54 升なので t の基準を 2.5 に上げる）
  A: OOS の純損益の平均 > 0 かつ Newey-West（ラグ H）の t ≥ 2.5
  B: IS の純損益の平均 > 0
  C: 年率 Sharpe（H 通りのずらしで重ならない系列を作り、その中央値。2019〜 全期間）が LS ≥ 1.0 / LT ≥ 0.5
  D: 隣の H（同じモデル・形）の少なくとも 1 本が B を満たし、OOS の平均 > 0（孤立した升は採らない）
  A〜D をすべて満たす升が「採用候補」。候補が出ても実運用の籠（資金・DD・建玉の上限）は別の検証で決める。
  1 升も満たさなければ、この系統（5 位・2 位の形）は不採用。
■ 副の問い（採否に使わない、報告だけ）
  LambdaRank は効くか: M3 − M1 の日ごとの差を、同じ H・形で OOS と全期間の Newey-West t で。
  予測の IC（予測とアルファの日ごとの順位相関）の平均。
"""

import argparse
import warnings
import time

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403
from scipy.stats import norm  # noqa: E402
from lightgbm import LGBMRanker, LGBMRegressor, early_stopping, log_evaluation  # noqa: E402

from dt_lgbm_train import INNER_VALID_DAYS, KW  # noqa: E402

warnings.filterwarnings("ignore")

HS = [1, 5, 10, 20]
N = 20
TOP = 500
EMBARGO = 5
TRAIN_START = pd.Timestamp('2017-07-01')
FOLD_YEARS = list(range(2019, 2027))
COST_LS, COST_LT, BORROW = 31.4e-4, 15.7e-4, 0.031
TAIL = 0.125
SEEDS = [0, 1, 2]
RANKED = ['mr2', 'mom131_25', 'vol231', 'adv11', 'price', 'ret20', 'ret40', 'ret60', 'vol20', 'vol40', 'vol60',
          'magap20', 'magap40', 'magap60', 'ret1', 'intraday']
MARKET = ['ave_mr2', 'ave_mom']
FEATS = RANKED + MARKET
RULES = {'R1': 'mr2', 'R2': 'ret1', 'R3': 'intraday'}


def load():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, C, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    cap = px.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays)

    # 時点母集団: 月末の master ∩ 時価総額上位 500 → 翌月に有効
    master = c.execute("SELECT Date, Code, Mrgn FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
    master['Date'] = pd.to_datetime(master.Date)
    msnap = {d: g.set_index('Code').Mrgn for d, g in master.groupby('Date')}
    mdays = sorted(msnap)
    mends = pd.Series(tdays, index=tdays).groupby(tdays.to_period('M')).last()
    elig = pd.DataFrame(False, index=tdays, columns=cap.columns)
    short = pd.DataFrame(False, index=tdays, columns=cap.columns)
    for k in range(len(mends) - 1):
        me, nx = mends.iloc[k], mends.iloc[k + 1]
        prior = [d for d in mdays if d <= me]
        if not prior:
            continue
        mg = msnap[prior[-1]]
        top = cap.loc[me]
        top = top[top.index.isin(mg.index)].dropna().sort_values(ascending=False).head(TOP).index
        rows = (tdays > me) & (tdays <= nx)
        elig.loc[rows, top] = True
        sh = [x for x in top if str(mg.get(x)) == '2']
        short.loc[rows, sh] = True
    codes = elig.columns[elig.any()]
    elig, short = elig[codes], short[codes]
    px = px[px.Code.isin(set(codes))]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(index=tdays, columns=codes)
         for k in ['AdjO', 'AdjC', 'C', 'Va']}
    return tp, tdays, codes, elig, short, W


def build(tp, tdays, codes, elig, short, W):
    O, C = W['AdjO'], W['AdjC']
    r = C.pct_change(fill_method=None)
    F = {
        'mr2': C / C.shift(2) - 1, 'mom131_25': C.shift(25) / C.shift(156) - 1,
        'vol231': r.rolling(231, min_periods=150).std(), 'adv11': W['Va'].rolling(11, min_periods=8).mean(),
        'price': W['C'], 'ret1': r, 'intraday': C / O - 1,
    }
    for n in (20, 40, 60):
        F[f'ret{n}'] = C / C.shift(n) - 1
        F[f'vol{n}'] = r.rolling(n, min_periods=int(n * 0.75)).std()
        F[f'magap{n}'] = C / C.rolling(n, min_periods=int(n * 0.75)).mean() - 1
    Of = O.ffill(limit=30)
    Y = {h: Of.shift(-(1 + h)) / O.shift(-1) - 1 for h in HS}
    Y['jpx'] = C.shift(-2) / C.shift(-1) - 1
    tO = tp.open
    TY = {h: tO.shift(-(1 + h)) / tO.shift(-1) - 1 for h in HS}
    TY['jpx'] = tp.close.shift(-2) / tp.close.shift(-1) - 1

    ti, ci = np.nonzero(elig.values)
    df = pd.DataFrame({'d': tdays[ti], 'code': codes[ci], 'short': short.values[ti, ci]})
    for k, v in F.items():
        df[k] = v.values[ti, ci]
    for k, v in Y.items():
        df[f'y_{k}'] = v.values[ti, ci]
        df[f't_{k}'] = TY[k].reindex(tdays).values[ti]
    df = df.replace([np.inf, -np.inf], np.nan)
    g = df.groupby('d')
    df['ave_mr2'] = g['mr2'].transform('mean')
    df['ave_mom'] = g['mom131_25'].transform('mean')
    for k in RANKED:
        df[f'rk_{k}'] = g[k].rank(pct=True).fillna(0.5)
    return df


def labels(df, h):
    y = df[f'y_{h}']
    g = y.groupby(df.d)
    pct = g.rank(pct=True)
    n = g.transform('count')
    z = norm.ppf(((pct * n) - 0.5) / n)
    return pct, z


def fit_predict(model, seed, tr, te, h):
    X = [f'rk_{k}' for k in RANKED] + MARKET
    pct, z = labels(tr, h)
    ok = pct.notna()
    if model == 'M2':
        ok &= (pct <= TAIL) | (pct >= 1 - TAIL)
    tr, pct, z = tr[ok], pct[ok], z[ok.values]
    days = np.array(sorted(tr.d.unique()))
    core = (tr.d < days[len(days) - INNER_VALID_DAYS]).values
    kw = dict(KW, random_state=seed)
    cb = [early_stopping(50, verbose=False), log_evaluation(0)]
    if model == 'M3':
        lab = np.minimum((pct * 10).astype(int), 9).values
        kw.update(objective='lambdarank', label_gain=list(range(10)))
        grp = lambda m: tr.d[m].groupby(tr.d[m], sort=False).size().values  # noqa: E731
        m = LGBMRanker(n_estimators=2000, **kw)
        m.fit(tr[X][core], lab[core], group=grp(core), eval_set=[(tr[X][~core], lab[~core])],
              eval_group=[grp(~core)], eval_at=[N], callbacks=cb)
        best = m.best_iteration_ or 200
        f = LGBMRanker(n_estimators=best, **kw)
        f.fit(tr[X], lab, group=grp(np.ones(len(tr), bool)))
    else:
        m = LGBMRegressor(n_estimators=2000, **kw)
        m.fit(tr[X][core], z[core], eval_set=[(tr[X][~core], z[~core])], eval_metric='l2', callbacks=cb)
        best = m.best_iteration_ or 200
        f = LGBMRegressor(n_estimators=best, **kw)
        f.fit(tr[X], z)
    p = pd.Series(f.predict(te[X]), index=te.index)
    return p.groupby(te.d).rank(pct=True), best


def walk(df, h, models, seeds, years):
    tdays = np.array(sorted(df.d.unique()))
    out = []
    for y in years:
        start = pd.Timestamp(f'{y}-01-01')
        te = df[(df.d >= start) & (df.d < pd.Timestamp(f'{y + 1}-01-01'))]
        if te.empty:
            continue
        prior = tdays[tdays < start]
        cut = prior[-(h + EMBARGO)]
        tr = df[(df.d >= TRAIN_START) & (df.d < cut)]
        sc = pd.DataFrame(index=te.index)
        for mdl in models:
            t0 = time.time()
            ps, bests = [], []
            for s in seeds:
                p, b = fit_predict(mdl, s, tr, te, h)
                ps.append(p)
                bests.append(b)
            sc[mdl] = sum(ps) / len(ps)
            print(f'  H={h} {y} {mdl} trees={bests} rows={len(tr)} {time.time() - t0:.0f}s', flush=True)
        out.append(sc)
    return pd.concat(out)


def daily(df, score, yk, h):
    """1 日 1 籠の保有期間あたり純損益（LS・LT）と IC。"""
    d = df[['d', 'short', f'y_{yk}', f't_{yk}']].copy()
    d['s'] = score
    d = d[d.s.notna() & d[f'y_{yk}'].notna()]
    rows = []
    for day, g in d.groupby('d'):
        g = g.sort_values('s', ascending=False)
        y = g[f'y_{yk}']
        rl = y.iloc[:N].mean()
        sh = g[g.short]
        rs = sh[f'y_{yk}'].iloc[-N:].mean()
        ic = g.s.corr(y - y.mean(), method='spearman')
        hh = 1 if yk == 'jpx' else h
        rows.append((day, rl - rs - COST_LS - BORROW * hh / 245, rl - g[f't_{yk}'].iloc[0] - COST_LT, ic))
    return pd.DataFrame(rows, columns=['d', 'LS', 'LT', 'ic']).set_index('d')


def nw_t(x, lag):
    x = np.asarray(x, float)
    x = x[~np.isnan(x)]
    n = len(x)
    if n < 20:
        return np.nan
    e = x - x.mean()
    s = e @ e / n
    for k in range(1, lag + 1):
        s += 2 * (1 - k / (lag + 1)) * (e[k:] @ e[:-k]) / n
    return x.mean() / np.sqrt(s / n)


def sharpe(x, h):
    x = x.dropna()
    v = [x.iloc[o::h] for o in range(h)]
    return float(np.median([s.mean() / s.std() * np.sqrt(245 / h) for s in v if len(s) > 10 and s.std() > 0]))


def summarize(D):
    rows = []
    for (name, h, form), x in D.items():
        is_, oos = x[x.index <= IS_END], x[x.index > IS_END]
        lag = 1 if h == 'jpx' else h
        rows.append(dict(name=name, H=h, form=form, is_bp=is_.mean() * 1e4, oos_bp=oos.mean() * 1e4,
                         oos_t=nw_t(oos, lag), all_t=nw_t(x, lag), sharpe=sharpe(x, lag), n_oos=len(oos)))
    s = pd.DataFrame(rows)
    s['A'] = (s.oos_bp > 0) & (s.oos_t >= 2.5)
    s['B'] = s.is_bp > 0
    s['C'] = np.where(s.form == 'LS', s.sharpe >= 1.0, s.sharpe >= 0.5)
    ok = (s.B & (s.oos_bp > 0)).values
    key = {(r.name, r.H, r.form): ok[i] for i, r in enumerate(s.itertuples())}
    def nb(r):  # noqa: E306
        if r.H == 'jpx':
            return False
        i = HS.index(r.H)
        return any(key.get((r.name, HS[j], r.form), False) for j in (i - 1, i + 1) if 0 <= j < len(HS))
    s['D'] = s.apply(nb, axis=1)
    s['pass'] = s.A & s.B & s.C & s.D
    return s


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--prelim', action='store_true')
    a = ap.parse_args()
    hs, seeds, years = (HS, SEEDS, FOLD_YEARS) if not a.prelim else ([5], [0], [2019, 2020])
    if a.prelim:
        print('*** 予備（コードの確認用。採否に使わない） ***')
    t0 = time.time()
    df = build(*load())
    print(f'rows {len(df)}  days {df.d.nunique()}  {time.time() - t0:.0f}s', flush=True)
    D, IC, preds = {}, {}, []
    first = pd.Timestamp(f'{years[0]}-01-01')
    last = pd.Timestamp(f'{years[-1] + 1}-01-01')
    ev = df[(df.d >= first) & (df.d < last)]
    for h in hs:
        sc = walk(df, h, ['M1', 'M2', 'M3'], seeds, years)
        sc['d'] = df.loc[sc.index, 'd']
        sc['code'] = df.loc[sc.index, 'code']
        sc['H'] = h
        preds.append(sc)
        for name in ['M1', 'M2', 'M3']:
            r = daily(ev, sc[name].reindex(ev.index), h, h)
            IC[(name, h)] = r.ic.mean()
            for form in ('LS', 'LT'):
                D[(name, h, form)] = r[form]
        for name, col in RULES.items():
            r = daily(ev, -ev[col], h, h)
            IC[(name, h)] = r.ic.mean()
            for form in ('LS', 'LT'):
                D[(name, h, form)] = r[form]
    if 1 in hs:
        for name, col in RULES.items():
            r = daily(ev, -ev[col], 'jpx', 1)
            for form in ('LS', 'LT'):
                D[(name + '_jpx', 'jpx', form)] = r[form]
    tag = '_prelim' if a.prelim else ''
    pd.concat(preds).to_parquet(f'{OUT}/jpx_wf_preds{tag}.parquet')
    pd.DataFrame({f'{k[0]}|{k[1]}|{k[2]}': v for k, v in D.items()}).to_parquet(f'{OUT}/jpx_wf_daily{tag}.parquet')
    s = summarize(D)
    s['ic'] = [IC.get((n, h), np.nan) for n, h in zip(s.name, s.H)]
    s.to_csv(f'{OUT}/jpx_wf_summary{tag}.csv', index=False)
    pd.set_option('display.width', 200)
    print(s.round(3).to_string(index=False))
    print('\n--- 副: M3（LambdaRank）− M1 の差（bp/籠、NW t） ---')
    for h in hs:
        for form in ('LS', 'LT'):
            x = (D[('M3', h, form)] - D[('M1', h, form)]).dropna()
            oos = x[x.index > IS_END]
            print(f'H={h:>2} {form}: 全 {x.mean() * 1e4:+.2f} (t {nw_t(x, h):+.2f})  OOS {oos.mean() * 1e4:+.2f} (t {nw_t(oos, h):+.2f})')
    print(f'\n採用候補: {int(s["pass"].sum())} 升  / 所要 {time.time() - t0:.0f}s')


if __name__ == '__main__':
    main()
