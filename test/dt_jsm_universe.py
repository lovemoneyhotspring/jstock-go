"""daytrade ロングの母集団に、JPX日経中小型株指数・JPX日経400 の構成銘柄かどうかを使えるか。

根拠: vault 20-research/2026-09-jp-dt-index-members.md
（中小型株指数は ROE・営業利益・ガバナンスで選んだ「質の良い小型株」の一覧。質の良い銘柄は、悪材料でない下げで戻りやすいか）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/dt_jsm_universe.py > test/out/dt_jsm_universe.txt 2>&1

■ 材料
  候補表 test/out/dt_candidates.parquet（本番のロングの母集団の規則: プライム・売買代金 1 億以上・時価総額の下 3 分位を除く・
  決算／信用規制／赤字を除く・ギャップ [-1, 0)・ストップ安でない。2026-09-20 に作ったもの）。
  構成銘柄: test/data/jpx_index_members.csv（定期入替の実施日から次の実施日の前日まで。非定期の除外は反映しない）。
  期間: 2017-08-31（構成銘柄の表がある最初の日）〜 候補表の最後の日。
■ 選定の模擬（簡略。本番の建可能額・1 単元・LBZ2 は入れない）
  毎日、並べ順の鍵 key_sort（ギャップ ÷ ボラ、小さいほど上）で並べ、上位 10 本を等金額で買い、寄り→引けで返す。
  銘柄の損益 = y_raw − dt_wf_target.liq_cost_bp(turnover_med)。日の値 = 選んだ銘柄の平均。
■ 検定（2 本、それぞれ）
  T1 中小型を先に: 上位 20 本のうち中小型の構成銘柄を先に（その中は key_sort 順）、残りを key_sort 順に取って 10 本。
  T2 400 を先に:   同じ形で JPX日経400 の構成銘柄を先に。
  差 = その形の日の値 − 基準（上位 10 本）の日の値。選ぶ銘柄が同じ日は差 0 として含める。
■ 採否（T1・T2 それぞれ）
  1: 差の全期間の平均 > 0、日の t ≥ 2.0
  2: IS（〜2022-12）・OOS（2023-01〜）とも差の平均 > 0
  満たせば、本番の形（dt_nscale.alloc_rule・建可能額・LBZ2 の並べ順・売買代金 5 億）で測り直す事前登録に進む。
■ 報告だけ
  上位 10 本の中で「構成銘柄 − 非構成銘柄」の損益の差（両方がいる日だけ、日で束ねる）、
  売買代金 5 億以上に絞った同じ差、ギャップの深さ 3 分位ごとの差、構成銘柄を外す形（上位 20 本から非構成を先に）。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from dt_wf_target import liq_cost_bp  # noqa: E402

TOP, POOL = 10, 20
START = pd.Timestamp('2017-08-31')


def tstat(x):
    x = pd.Series(x).dropna()
    return x.mean() / x.std(ddof=1) * np.sqrt(len(x)) if len(x) > 2 and x.std(ddof=1) > 0 else np.nan


def main():
    d = pd.read_parquet(f'{OUT}/dt_candidates.parquet', columns=['d', 'code', 'gap', 'y_raw', 'key_sort', 'turnover_med'])
    d['d'] = pd.to_datetime(d.d)
    d = d[d.d >= START].copy()
    d['c4'] = d.code.astype(str).str[:4]
    m = pd.read_csv(f'{OUT}/../data/jpx_index_members.csv', dtype=str)
    m['start'], m['end'] = pd.to_datetime(m.start), pd.to_datetime(m.end)
    for idx in ('jsm', 'j400'):
        flag = np.zeros(len(d), bool)
        for s, g in m[m['index'] == idx].groupby('start'):
            flag |= ((d.d >= s) & (d.d <= g.end.iloc[0])).values & d.c4.isin(set(g.code)).values
        d[idx] = flag
    d['pnl'] = d.y_raw - liq_cost_bp(d.turnover_med.fillna(0).values) / 1e4
    d['r'] = d.groupby('d').key_sort.rank(method='first')
    pool = d[d.r <= POOL].copy()

    def day_val(sel):
        return sel.groupby('d').pnl.mean() * 1e4

    base = day_val(pool[pool.r <= TOP])

    def tilt(flag, prefer=True):
        p = pool.copy()
        p['pri'] = np.where(p[flag] == prefer, 0, 1)
        p = p.sort_values(['d', 'pri', 'r'])
        p['k'] = p.groupby('d').cumcount()
        return day_val(p[p.k < TOP])

    pd.set_option('display.width', 200)
    print(f'日数 {base.size}  基準（上位 10 本）の日の平均 {base.mean():+.2f} bp')
    for name, flag in (('T1 中小型を先に', 'jsm'), ('T2 400 を先に', 'j400')):
        v = tilt(flag)
        diff = (v - base).reindex(base.index).fillna(0)
        isx, oos = diff[diff.index <= IS_END], diff[diff.index > IS_END]
        t = tstat(diff)
        chg = (diff != 0).mean()
        yr = diff.groupby(diff.index.year).mean().round(2).to_dict()
        c1, c2 = diff.mean() > 0 and t >= 2.0, isx.mean() > 0 and oos.mean() > 0
        print(f'\n=== {name}: 差 {diff.mean():+.2f} bp/日  t {t:+.2f}  IS {isx.mean():+.2f}  OOS {oos.mean():+.2f}  '
              f'選定が変わった日 {chg:.0%}')
        print('  年の平均:', yr)
        print(f'  判定: 1 {c1}  2 {c2}  → {"本番の形で測り直す" if c1 and c2 else "不採用"}')

    print('\n=== 報告: 上位 10 本の中で 構成銘柄 − 非構成銘柄（両方がいる日、日で束ねる、bp）')
    t10 = pool[pool.r <= TOP]
    for flag in ('jsm', 'j400'):
        for lab, x in (('全部', t10), ('売買代金 5 億以上', t10[t10.turnover_med >= 5e8])):
            g = x.groupby(['d', flag]).pnl.mean().unstack()
            if True not in g or False not in g:
                continue
            dd = (g[True] - g[False]).dropna() * 1e4
            print(f'  {flag} {lab}: {dd.mean():+.1f}（t {tstat(dd):+.2f}、{len(dd)} 日）  構成 {g[True].mean() * 1e4:+.1f} / 非構成 {g[False].mean() * 1e4:+.1f}')
        x = t10.copy()
        x['gq'] = x.groupby('d').gap.rank(pct=True).apply(lambda v: '深' if v <= 1 / 3 else ('中' if v <= 2 / 3 else '浅'))
        out = {}
        for q, y in x.groupby('gq'):
            g = y.groupby(['d', flag]).pnl.mean().unstack()
            if True in g and False in g:
                dd = (g[True] - g[False]).dropna() * 1e4
                out[q] = f'{dd.mean():+.1f}（t {tstat(dd):+.2f}）'
        print(f'  {flag} ギャップの深さ 3 分位ごと:', out)
        v = tilt(flag, prefer=False)
        diff = (v - base).reindex(base.index).fillna(0)
        print(f'  {flag} を後回しにする形: 差 {diff.mean():+.2f} bp/日（t {tstat(diff):+.2f}）')


if __name__ == '__main__':
    main()
