"""（事後の点検・採否に使わない）JPX日経中小型株指数の採用が発表後に TOPIX を +347 bp 上回った件の見直し。

根拠: vault 20-research/2026-09-jp-jsm-rebalance.md（報告だけの行で見つけた。結果を見てからの点検）
  - 同じ窓（発表の翌寄り → 組み入れの日の引け）で、採用と時価総額・売買代金が近い「入替と無関係の銘柄」の超過を並べる
  - 採用のうち、発表前 20 日の上げ・時価総額・売買代金で分けた超過
  - 上位数銘柄を除いた平均、年ごとの中央値

  bash test/heavy.sh test/.venv/bin/python test/jsm_add_control.py > test/out/jsm_add_control.txt 2>&1
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from jsm_rebalance import EVENTS  # noqa: E402
from j400_rebalance import EVENTS as J400  # noqa: E402


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va', 'MktCap']}
    va = W['Va'].rolling(20, min_periods=10).median()
    out = []
    for ann, eff, adds, dels in EVENTS:
        a0 = int(np.searchsorted(tdays, pd.Timestamp(ann), side='right')) - 1
        a, b = a0 + 1, int(np.searchsorted(tdays, pd.Timestamp(eff), side='left')) - 1
        ex = (W['AdjC'].iloc[b] / W['AdjO'].iloc[a] - 1) - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
        p0 = (W['AdjC'].iloc[a0] / W['AdjC'].iloc[a0 - 20] - 1) - (tp.close.iloc[a0] / tp.close.iloc[a0 - 20] - 1)
        mc, v = W['MktCap'].iloc[a0], va.iloc[a0]
        j = [x for x in J400 if x[0] == ann][0]
        touched = {k + '0' for k in (adds + dels + j[2] + j[3]).split()}
        addc = [k + '0' for k in adds.split() if k + '0' in ex.index]
        d = pd.DataFrame({'ex': ex, 'p0': p0, 'mc': mc, 'va': v}).dropna()
        A = d.loc[d.index.isin(addc)]
        # 採用の時価総額・売買代金の 10〜90% の範囲にある、入替と無関係の銘柄
        lo, hi = A.mc.quantile(.1), A.mc.quantile(.9)
        vlo = A.va.quantile(.1)
        C = d[(~d.index.isin(touched)) & d.mc.between(lo, hi) & (d.va >= vlo)]
        out.append(dict(ann=ann, n_add=len(A), add_bp=A.ex.mean() * 1e4, add_med=A.ex.median() * 1e4,
                        add_trim=A.ex.sort_values().iloc[3:-3].mean() * 1e4,
                        n_ctl=len(C), ctl_bp=C.ex.mean() * 1e4, ctl_med=C.ex.median() * 1e4))
        A = A.assign(ann=ann)
        out[-1]['A'] = A
    r = pd.DataFrame([{k: v for k, v in o.items() if k != 'A'} for o in out]).set_index('ann')
    r['diff'] = r.add_bp - r.ctl_bp
    pd.set_option('display.width', 200)
    print(r.round(1).to_string())
    x = r['diff']
    print(f'\n採用 − 対照の平均 {x.mean():+.1f} bp  t {x.mean() / x.std(ddof=1) * np.sqrt(len(x)):+.2f}  正 {(x > 0).sum()}/{len(x)}')
    A = pd.concat([o['A'] for o in out])
    for k in ('p0', 'mc', 'va'):
        A['q'] = A.groupby('ann')[k].transform(lambda s: pd.qcut(s.rank(method='first'), 3, labels=['低', '中', '高']))
        print(f'{k} の三分位ごとの採用の超過（bp）:', (A.groupby('q', observed=True).ex.mean() * 1e4).round(1).to_dict())


if __name__ == '__main__':
    main()
