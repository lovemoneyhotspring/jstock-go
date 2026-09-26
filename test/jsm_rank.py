"""JPX日経中小型株指数の採用から 20 銘柄だけ買うとき、どう並べるか（人が別の証券会社で買える上限が 20 銘柄のため）。

根拠: vault 20-research/2026-09-jp-jsm-rank.md（元は vault 20-research/2026-09-jp-jsm-rebalance.md の事後の発見）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから並べ方・基準を変えない。
注意: 採用の効き自体が事後の発見で、9 回すべてをもう見ている。並べ方の比較も同じ 9 回の上でしかできない。
      だから候補は少数に絞り、基準は厳しめにし、どれも通らなければ「実務で困らない並べ方」を使う。

  bash test/heavy.sh test/.venv/bin/python test/jsm_rank.py > test/out/jsm_rank.txt 2>&1

■ 母集団と損益（jsm_add_bias.py の手取りと同じ）
  各回の対象（中小型の採用のうち JPX日経400 の入替と重ならない銘柄）。
  損益 = 発表の翌寄り → 組み入れの日の引けの TOPIX 超過 − 流動性別コスト − 滑り 10 bp。
  並べる材料はすべて発表日の引けまでに分かるもの。
■ 並べ方（上位 20 銘柄を等金額で買う）
  R1 売買代金（20 日中央値）の大きい順
  R2 時価総額の大きい順
  R3 時価総額の小さい順
  R4 需給の圧力（時価総額 ÷ 売買代金、指数の買いが 1 日の売買の何日分か）の大きい順
  R5 発表前 20 営業日の TOPIX 超過の小さい順（発表前に上げていない銘柄）
  R6 ROE（直近の本決算）の高い順
■ 指標
  差 = 上位 20 銘柄の平均 − その回の対象全部の平均（「全部買えたら」との差）。9 回で平均と t を取る。
■ 採否（候補が 6 本あるので t の基準を上げる）
  1: 差の平均 > 0、t ≥ 2.5
  2: 9 回中 7 回以上で差 > 0
  3: IS（〜2022 の 5 回）・OOS（2023〜の 4 回）とも差の平均 > 0
  通った並べ方が複数なら差の平均が大きいもの。どれも通らなければ R1（売買代金の大きい順）を使う
  ——効きが変わらないなら、滑りが小さく約定しやすい銘柄を選ぶのが実務で得（ただし R1 の差の t が −1.0 より悪ければ R2）。
  R1 は、事後の点検で「売買代金の多い採用ほど効きが大きい」（三分位 +178 / +284 / +432）を見た後に入れた候補なので、
  R1 が通っても前向きの 2 回（2027・2028）で確かめるまでは確定としない。
■ 報告だけ
  上位 10・30 銘柄での同じ差、各並べ方の上位 20 の手取りの回ごとの値、上位 20 に入る銘柄の売買代金の最小値。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from dt_wf_target import liq_cost_bp  # noqa: E402
from jsm_rebalance import EVENTS  # noqa: E402
from j400_rebalance import EVENTS as J400  # noqa: E402

SLIP_BP = 10.0
TOP = 20


def tstat(x):
    x = pd.Series(x).dropna()
    return x.mean() / x.std(ddof=1) * np.sqrt(len(x))


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va', 'MktCap']}
    va = W['Va'].rolling(20, min_periods=10).median()
    fs = c.execute("""SELECT DiscDate, Code, CurPerType, TRY_CAST(NP AS DOUBLE) NP, TRY_CAST(Eq AS DOUBLE) Eq FROM fins""").df()
    fs['DiscDate'] = pd.to_datetime(fs.DiscDate)
    fy = fs[fs.CurPerType == 'FY'].dropna(subset=['NP', 'Eq']).sort_values('DiscDate')
    rows = []
    for ann, eff, adds, dels in EVENTS:
        a0 = int(np.searchsorted(tdays, pd.Timestamp(ann), side='right')) - 1
        a, b = a0 + 1, int(np.searchsorted(tdays, pd.Timestamp(eff), side='left')) - 1
        j = [x for x in J400 if x[0] == ann][0]
        ov = set(j[2].split()) | set(j[3].split())
        f = fy[fy.DiscDate <= tdays[a0]].groupby('Code').last()
        for k in adds.split():
            code = k + '0'
            if k in ov or code not in W['AdjC'].columns:
                continue
            O, C = W['AdjO'][code].values, W['AdjC'][code].values
            if not (np.isfinite(O[a]) and np.isfinite(C[b])):
                continue
            ex = (C[b] / O[a] - 1) - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
            v = va[code].values[a0]
            cost = (float(liq_cost_bp(np.nan_to_num(v))) + SLIP_BP) / 1e4
            mc = W['MktCap'][code].values[a0]
            p0 = (C[a0] / C[a0 - 20] - 1) - (tp.close.iloc[a0] / tp.close.iloc[a0 - 20] - 1) if a0 >= 20 else np.nan
            roe = (f.NP / f.Eq).get(code, np.nan)
            rows.append(dict(ann=ann, code=k, net=(ex - cost) * 1e4, va=v, mc=mc, press=mc / v if v else np.nan, p0=p0, roe=roe))
    d = pd.DataFrame(rows)
    d.to_csv(f'{OUT}/jsm_rank.csv', index=False)
    RANK = {'R1 売買代金の大きい順': ('va', False), 'R2 時価総額の大きい順': ('mc', False), 'R3 時価総額の小さい順': ('mc', True),
            'R4 時価総額÷売買代金の大きい順': ('press', False), 'R5 発表前 20 日の超過の小さい順': ('p0', True),
            'R6 ROE の高い順': ('roe', False)}
    allm = d.groupby('ann').net.mean()
    print('回ごとの対象数:', d.groupby('ann').size().to_dict())
    print(f'全部買った場合の手取り: 回の平均 {allm.mean():+.1f} bp（t {tstat(allm):+.2f}）')
    pd.set_option('display.width', 220)
    res, per = {}, {}
    for name, (col, asc) in RANK.items():
        out = {}
        for n in (10, 20, 30):
            top = d.dropna(subset=[col]).sort_values(['ann', col], ascending=[True, asc]).groupby('ann').head(n)
            out[n] = top.groupby('ann').net.mean()
        diff = (out[TOP] - allm).dropna()
        idx = pd.to_datetime(diff.index)
        isx, oos = diff[idx <= IS_END], diff[idx > IS_END]
        t = tstat(diff)
        c1, c2, c3 = diff.mean() > 0 and t >= 2.5, int((diff > 0).sum()) >= 7, isx.mean() > 0 and oos.mean() > 0
        res[name] = dict(diff=diff.mean(), t=t, pos=int((diff > 0).sum()), IS=isx.mean(), OOS=oos.mean(),
                         top20=out[TOP].mean(), d10=(out[10] - allm).mean(), d30=(out[30] - allm).mean(), ok=c1 and c2 and c3)
        per[name] = out[TOP]
    r = pd.DataFrame(res).T
    print('\n=== 上位 20 − 全部（bp、回の平均）')
    print(r.astype({'pos': int}).round(2).to_string())
    print('\n回ごとの上位 20 の手取り（bp）:')
    print(pd.DataFrame(per).assign(全部=allm).round(0).to_string())
    ok = r[r.ok]
    if len(ok):
        pick = ok.diff.astype(float).idxmax()
        why = '基準を通った中で差が最大'
    elif r.loc['R1 売買代金の大きい順', 't'] > -1.0:
        pick, why = 'R1 売買代金の大きい順', 'どれも基準を通らない → 既定の R1（滑りが小さい）'
    else:
        pick, why = 'R2 時価総額の大きい順', 'どれも基準を通らず R1 の t が −1.0 より悪い → R2'
    print(f'\n判定: {pick}（{why}）')


if __name__ == '__main__':
    main()
