"""（事後の探索・採否の基準は事前登録していない）中小型株指数の採用を売買代金の大きい順に買うとき、何銘柄に絞るのがよいか。

根拠: vault 20-research/2026-09-jp-jsm-rank.md の続き（ユーザの問い: 50 万 × 20 銘柄と 100 万 × 10 銘柄のどちらがよいか、2026-09-26）。
資金を一定（1,000 万円）にして銘柄数 N を変える。材料は jsm_rank.py の出力（test/out/jsm_rank.csv、銘柄ごとの手取り）。

  test/.venv/bin/python test/jsm_rank_n.py

出すもの（N = 5, 8, 10, 12, 15, 20, 25, 30）:
  回の手取りの平均・中央値・最悪・標準偏差・負けた回、全部との差の t、
  1 回の損益（1,000 万円）の平均と最悪、1 銘柄の最悪の手取りが回の損益に占める分、
  1 銘柄の金額が売買代金 20 日中央値の何 % か（最大）、
  1 回ずつ抜いたとき（9 通り）に「最もよい N」がどれだけ動くか。
"""
import os

import numpy as np
import pandas as pd

OUT = os.path.join(os.path.dirname(__file__), 'out')
CAP = 1e7
NS = (5, 8, 10, 12, 15, 20, 25, 30)


def main():
    d = pd.read_csv(f'{OUT}/jsm_rank.csv')
    d = d.dropna(subset=['va']).sort_values(['ann', 'va'], ascending=[True, False])
    allm = d.groupby('ann').net.mean()
    rows, per = [], {}
    for n in NS:
        top = d.groupby('ann').head(n)
        g = top.groupby('ann')
        m = g.net.mean()
        per[n] = m
        diff = m - allm
        worst_name = g.net.min()
        rows.append(dict(N=n, 一銘柄_万円=CAP / n / 1e4, 平均=m.mean(), 中央値=m.median(), 最悪=m.min(), 標準偏差=m.std(ddof=1),
                         負けた回=int((m < 0).sum()), 全部との差=diff.mean(), 差のt=diff.mean() / diff.std(ddof=1) * 3,
                         一回の損益_万円=m.mean() / 1e4 * CAP / 1e4, 最悪の回_万円=m.min() / 1e4 * CAP / 1e4,
                         最悪の一銘柄_bp=worst_name.min(), 一銘柄の最悪が回に効く_bp=(worst_name / n).min(),
                         売買代金比_最大pct=(CAP / n / top.va).max() * 100, 平均対標準偏差=m.mean() / m.std(ddof=1)))
    r = pd.DataFrame(rows).set_index('N')
    pd.set_option('display.width', 250)
    print(f'全部買った場合: 回の平均 {allm.mean():+.1f} bp')
    print(r.round(2).to_string())
    print('\n回ごとの手取り（bp）:')
    print(pd.DataFrame(per).round(0).assign(全部=allm.round(0)).to_string())
    # 1 回ずつ抜いたとき、平均が最大の N と、平均÷標準偏差が最大の N
    best_m, best_s = [], []
    anns = allm.index
    for a in anns:
        P = pd.DataFrame(per).drop(index=a)
        best_m.append(P.mean().idxmax())
        best_s.append((P.mean() / P.std(ddof=1)).idxmax())
    print('\n1 回ずつ抜いたときの最良の N（平均）:', dict(zip(anns, best_m)))
    print('1 回ずつ抜いたときの最良の N（平均÷標準偏差）:', dict(zip(anns, best_s)))


if __name__ == '__main__':
    main()
