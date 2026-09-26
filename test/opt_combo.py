"""225 プットのスプレッドを daytrade に小さく足すと、合算の Sharpe と DD は良くなるか。opt_spread.py の続き。

根拠: vault 20-research/2026-09-n225-option-spread.md（単独は Sharpe 0.44〜0.58 で不採用、daytrade と月次相関 −0.2 前後）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh bash -c "cd test && .venv/bin/python opt_combo.py" > test/out/opt_combo.txt 2>&1

■ 形
  daytrade: out/trades_10y.csv の日次損益（2026-09-09 時点の 10 年の再現）。資金 K = 200 万円（opt_premium と同じ）。
  オプション: opt_spread.run のプットのスプレッド 4 通り（d ∈ {5, 7}% × w ∈ {3, 5}%）。コンドルは入れない。
  数量: 1 か月の損失の上限 = f × K。f ∈ {0.05, 0.10, 0.20}。日次損益は opt_spread の C = 300 万円の値を f × K / C 倍する。
■ 指標（2016-09〜2026-08）
  合算の日次損益の Sharpe（年率、対 K）、最大 DD（円）、年率（対 K）。IS（〜2022）/ OOS（2023〜）の Sharpe。
■ 採否（f ごとに、4 通りの中央値で）
  1: 合算の Sharpe > daytrade 単独の Sharpe
  2: 合算の最大 DD ≤ daytrade 単独の最大 DD × 1.2
  3: IS・OOS の両方で合算の Sharpe ≥ daytrade 単独の Sharpe
  1〜3 を満たす f が 1 つでもあれば採用候補（最も大きい f を候補とし、実際の証拠金・枚数の単位を別に事前登録）。
  どの f も満たさなければ不採用。
"""

import sys
sys.path.insert(0, '.')
import itertools  # noqa: E402

import numpy as np  # noqa: E402
import pandas as pd  # noqa: E402

import opt_spread as S  # noqa: E402

K = 2_000_000


def stat(x):
    x = x.dropna()
    eq = x.cumsum()
    return dict(sharpe=x.mean() / x.std() * np.sqrt(245), dd=(eq - eq.cummax()).min(), annual=x.sum() / (len(x) / 245) / K * 100)


def main():
    t = pd.read_csv('out/trades_10y.csv')
    t['date'] = pd.to_datetime(t.date)
    dt = t.groupby('date').pnl.sum()
    opts = {}
    for d, w in itertools.product([0.05, 0.07], [0.03, 0.05]):
        daily, _ = S.run(d, w, False)
        opts[f'd{int(d * 100)} w{int(w * 100)}'] = daily
    idx = pd.DatetimeIndex(sorted(set(dt.index) | set().union(*[set(v.index) for v in opts.values()])))
    idx = idx[(idx >= '2016-09-01') & (idx <= '2026-08-31')]
    base = dt.reindex(idx).fillna(0)
    b, bi, bo = stat(base), stat(base[base.index <= S.IS_END]), stat(base[base.index > S.IS_END])
    print(f"daytrade 単独: Sharpe {b['sharpe']:.2f}（IS {bi['sharpe']:.2f} / OOS {bo['sharpe']:.2f}）"
          f"  DD {b['dd']:,.0f}  年率 {b['annual']:.1f}%")
    ok_any = []
    for f in (0.05, 0.10, 0.20):
        rows = []
        for lab, daily in opts.items():
            comb = base + daily.reindex(idx).fillna(0) * (f * K / S.CAP)
            s, si, so = stat(comb), stat(comb[comb.index <= S.IS_END]), stat(comb[comb.index > S.IS_END])
            rows.append(dict(lab=lab, **s, is_sh=si['sharpe'], oos_sh=so['sharpe']))
            print(f"  f={f:.2f} {lab}: Sharpe {s['sharpe']:.2f}（IS {si['sharpe']:.2f} / OOS {so['sharpe']:.2f}）"
                  f"  DD {s['dd']:,.0f}  年率 {s['annual']:.1f}%")
        g = pd.DataFrame(rows)
        c1 = g.sharpe.median() > b['sharpe']
        c2 = g.dd.median() >= b['dd'] * 1.2
        c3 = g.is_sh.median() >= bi['sharpe'] and g.oos_sh.median() >= bo['sharpe']
        print(f"f={f:.2f} 中央値: Sharpe {g.sharpe.median():.2f}  DD {g.dd.median():,.0f}  → 1 {c1}  2 {c2}  3 {c3}")
        if c1 and c2 and c3:
            ok_any.append(f)
    print(f"\n判定: {'採用候補 f=' + str(max(ok_any)) if ok_any else '不採用'}")


if __name__ == '__main__':
    main()
