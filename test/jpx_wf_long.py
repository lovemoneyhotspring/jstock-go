"""JPX Kaggle 5 位・2 位の LightGBM を、保有 40・60 日のロングのみ（TOPIX 比）で測る。jpx_wf.py の続き。

根拠: vault 20-research/2026-09-jp-jpx-kaggle-wf.md の結論（M1 H=20 LT だけが IS +26.0 / OOS +18.0 bp/籠、費用前 +37.8）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/jpx_wf_long.py > test/out/jpx_wf_long.txt 2>&1

■ jpx_wf.py から変えないもの
  母集団（月末の時価総額上位 500、時点で作る）、特徴量 18 本、M1（回帰・アルファの z）と M2（端だけで学習）、
  3 シード、暦年の fold（2019〜2026、学習は 2017-07 から、末尾 H+5 営業日を捨てる）、上位 20 銘柄の等金額、
  t+1 の寄りで建て t+1+H の寄りで返す、毎日 1 籠で H 本が並走、IS = 2019〜2022 / OOS = 2023-01〜。
■ 変えるもの
  H ∈ {20, 40, 60}。H ごとにその H の目的変数で学習し直す。
  判定する形は LT（ロング − TOPIX − 往復 15.7 bp）だけ。LS は参考に出す。
  LambdaRank（M3）は jpx_wf で効かなかったので入れない。
■ 採否（判定する升は M1・M2 × H=40・60 の 4 升。H=20 は jpx_wf で判定済みなので D の隣としてだけ使う）
  A: OOS の平均 > 0 かつ Newey-West（ラグ H）の t ≥ 2.0
  B: IS の平均 > 0
  C: 年率の情報比（H 通りのずらしで重ならない系列を作り、その中央値。2019〜 全期間）≥ 0.5
  D: 隣の H（同じモデル）が B を満たし、OOS の平均 > 0
  E: 最も良かった 1 年（信号日の暦年）を除いても全期間の平均 > 0
  A〜E をすべて満たせば採用候補。そのときは実運用の籠（月次の組み直し・資金・DD）を別の事前登録で測る。
  4 升とも満たさなければ、JPX Kaggle 系の保有を伸ばす方向は打ち切る。
■ 報告だけ（採否に使わない）
  費用前の優位（bp/籠と bp/日）、暦年ごとの平均、LS の同じ表。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
import time  # noqa: E402

import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

import jpx_wf as J  # noqa: E402

J.HS = [20, 40, 60]
MODELS = ['M1', 'M2']
JUDGE_H = [40, 60]


def main():
    t0 = time.time()
    df = J.build(*J.load())
    print(f'rows {len(df)}  days {df.d.nunique()}  {time.time() - t0:.0f}s', flush=True)
    ev = df[df.d >= pd.Timestamp('2019-01-01')]
    D, preds = {}, []
    for h in J.HS:
        sc = J.walk(df, h, MODELS, J.SEEDS, J.FOLD_YEARS)
        sc['d'], sc['code'], sc['H'] = df.loc[sc.index, 'd'], df.loc[sc.index, 'code'], h
        preds.append(sc)
        for m in MODELS:
            r = J.daily(ev, sc[m].reindex(ev.index), h, h)
            D[(m, h, 'LT')], D[(m, h, 'LS')] = r.LT, r.LS
    pd.concat(preds).to_parquet(f'{J.OUT}/jpx_wf_long_preds.parquet')
    pd.DataFrame({f'{k[0]}|{k[1]}|{k[2]}': v for k, v in D.items()}).to_parquet(f'{J.OUT}/jpx_wf_long_daily.parquet')

    rows = []
    for (m, h, form), x in D.items():
        x = x.dropna()
        is_, oos = x[x.index <= J.IS_END], x[x.index > J.IS_END]
        yr = x.groupby(x.index.year).mean()
        cost = J.COST_LT if form == 'LT' else J.COST_LS + J.BORROW * h / 245
        rows.append(dict(model=m, H=h, form=form, is_bp=is_.mean() * 1e4, oos_bp=oos.mean() * 1e4,
                         oos_t=J.nw_t(oos, h), ir=J.sharpe(x, h), ex_best=x[x.index.year != yr.idxmax()].mean() * 1e4,
                         gross_bp=(x.mean() + cost) * 1e4, gross_bp_day=(x.mean() + cost) * 1e4 / h,
                         **{str(y): v * 1e4 for y, v in yr.items()}))
    s = pd.DataFrame(rows)
    s['A'] = (s.oos_bp > 0) & (s.oos_t >= 2.0)
    s['B'] = s.is_bp > 0
    s['C'] = s.ir >= 0.5
    ok = {(r.model, r.H, r.form): (r.is_bp > 0) and (r.oos_bp > 0) for r in s.itertuples()}
    s['D'] = [any(ok.get((r.model, J.HS[j], r.form), False) for j in (J.HS.index(r.H) - 1, J.HS.index(r.H) + 1)
                  if 0 <= j < len(J.HS)) for r in s.itertuples()]
    s['E'] = s.ex_best > 0
    s['judged'] = (s.form == 'LT') & s.H.isin(JUDGE_H)
    s['pass'] = s.judged & s.A & s.B & s.C & s.D & s.E
    s.to_csv(f'{J.OUT}/jpx_wf_long_summary.csv', index=False)
    pd.set_option('display.width', 250)
    print(s.round(2).to_string(index=False))
    print(f'\n採用候補: {int(s["pass"].sum())} 升 / 所要 {time.time() - t0:.0f}s')


if __name__ == '__main__':
    main()
