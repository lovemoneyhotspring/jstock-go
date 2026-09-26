"""225 オプションの売りに損失の上限を付ける（プットのスプレッド・アイアンコンドル）。opt_premium.py の続き。

根拠: vault 20-research/2026-09-n225-option-spread.md（親: 2026-09-n225-option-premium。素の売りは Sharpe 0.29〜0.48、
1 枚 300 万円に対し最大 DD −600〜−909 万円で不採用）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  cd test && ../test/.venv/bin/python opt_spread.py > out/opt_spread.txt 2>&1

■ opt_premium.py から変えないもの
  データ（derivatives_bars_daily_options_225 の清算値）、建て日 = SQ の翌営業日、限月 = 次の SQ、SQ で本質価値で決済
  （SQ 値は ^N225 の SQ 日の始値で近似）、日次は清算値で評価、費用 = 1 枚・1 脚あたり往復 500 円＋建て値 1 ティック（5 円）不利。
■ 形
  売り: 建て日の原資産 × (1 − d) 以下で最も近いプット（コンドルは × (1 + d) 以上で最も近いコールも）
  買い: 売りの行使価格 − 原資産 × w 以下で最も近いプット（コールは + 原資産 × w 以上で最も近いコール）
  格子: d ∈ {5, 7}% × w ∈ {3, 5}% × {プットのスプレッド, アイアンコンドル} の 8 通り
■ 数量（損失の上限で資金を揃える）
  資金 C = 300 万円。各月の枚数 = C ÷ （幅（行使価格の差）× 1,000 円）。端数の枚数を許す（紙の上の比較）。
  コンドルは片側しか同時に損しないので、プット側の幅で枚数を決める。
■ 指標（2016-09〜2026-08）
  年率（対 C）、日次 Sharpe（年率）、最大 DD（対 C）、最悪の月、IS（〜2022）/ OOS（2023〜）の年率、
  daytrade（out/trades_10y.csv）との月次相関。
■ 採否
  1: 8 通りの Sharpe の中央値 ≥ 0.6
  2: 最大 DD の中央値 ≤ C の 40%
  3: 中央値の格子で IS・OOS とも年率 > 0（中央値の格子 = Sharpe が中央に近い 2 通りの両方）
  4: daytrade との月次相関の中央値 ≤ 0.1
  すべて満たせば採用候補（資金・証拠金・約定の実際を別の事前登録で測る）。満たさなければ不採用。
"""

import sys
sys.path.insert(0, '.')
from common import *  # noqa: E402,F403
import itertools  # noqa: E402

c = con()
o = c.execute(f"""SELECT Date, SQD, PCDiv, TRY_CAST(Strike AS DOUBLE) K, TRY_CAST(Settle AS DOUBLE) S, TRY_CAST(UnderPx AS DOUBLE) U
FROM read_parquet('{ROOT}/derivatives_bars_daily_options_225/*.parquet', union_by_name=true)""").df()
for k in ['Date', 'SQD']:
    o[k] = pd.to_datetime(o[k])
o = o[o.S.notna() & (o.K > 0)]
tdays = sorted(o.Date.unique())
n225 = wb('^N225')
sqs = [s for s in sorted(o.SQD.unique()) if pd.Timestamp('2016-09-01') <= s <= pd.Timestamp('2026-08-31')]
CAP = 3_000_000
MULT = 1000
FEE = 500
TICK = 5
idx = o.set_index(['SQD', 'PCDiv', 'K']).sort_index()


def sq_value(sqd):
    if sqd in n225.index:
        return n225.open[sqd]
    x = o[o.Date == sqd].U
    return x.iloc[0] if len(x) else np.nan


def leg_pnl(nxt, pc, K, e, sqv, sign):
    """sign = −1 売り / +1 買い。1 枚の日次損益（円）。"""
    try:
        path = idx.loc[(nxt, pc, K)]
    except KeyError:
        return None
    s = path[(path.Date >= e) & (path.Date <= nxt)].sort_values('Date').set_index('Date').S
    if s.empty or s.index[0] != e:
        return None
    entry = s.iloc[0] - TICK if sign < 0 else s.iloc[0] + TICK
    intrinsic = max(0.0, (K - sqv) if pc == '1' else (sqv - K)) if np.isfinite(sqv) else s.iloc[-1]
    vals, dates = list(s.values), list(s.index)
    vals.append(intrinsic)
    dates.append(nxt if nxt > dates[-1] else dates[-1] + pd.Timedelta(days=1))
    p = [sign * (vals[0] - entry) * MULT - FEE]
    p += [sign * (vals[j] - vals[j - 1]) * MULT for j in range(1, len(vals))]
    return pd.Series(p, index=dates).groupby(level=0).sum()


def run(d, w, condor):
    daily = pd.Series(0.0, index=pd.DatetimeIndex(tdays))
    months = []
    for i in range(len(sqs) - 1):
        sqd, nxt = sqs[i], sqs[i + 1]
        after = [t for t in tdays if t > sqd]
        if not after:
            break
        e = after[0]
        chain = o[(o.Date == e) & (o.SQD == nxt)]
        if chain.empty:
            continue
        U = chain.U.iloc[0]
        P, Cl = chain[chain.PCDiv == '1'], chain[chain.PCDiv == '2']
        ps = P[P.K <= U * (1 - d)].K.max()
        pb = P[P.K <= ps - U * w].K.max() if np.isfinite(ps) else np.nan
        if not (np.isfinite(ps) and np.isfinite(pb)):
            continue
        n = CAP / ((ps - pb) * MULT)
        legs = [('1', ps, -1), ('1', pb, +1)]
        if condor:
            cs = Cl[Cl.K >= U * (1 + d)].K.min()
            cb = Cl[Cl.K >= cs + U * w].K.min() if np.isfinite(cs) else np.nan
            if np.isfinite(cs) and np.isfinite(cb):
                legs += [('2', cs, -1), ('2', cb, +1)]
        sqv = sq_value(nxt)
        tot = 0.0
        ok = True
        parts = []
        for pc, K, sign in legs:
            r = leg_pnl(nxt, pc, K, e, sqv, sign)
            if r is None:
                ok = False
                break
            parts.append(r * n)
        if not ok:
            continue
        for r in parts:
            daily = daily.add(r, fill_value=0)
            tot += r.sum()
        months.append(dict(sq=nxt, pnl=tot, n=n))
    return daily[daily.index >= pd.Timestamp('2016-09-01')], pd.DataFrame(months)


def st(daily):
    r = daily / CAP
    eq = daily.cumsum()
    yrs = len(daily) / 245
    return dict(annual=daily.sum() / yrs / CAP * 100, sharpe=r.mean() / r.std() * np.sqrt(245),
                dd=(eq - eq.cummax()).min() / CAP * 100)


def main():
    t = pd.read_csv('out/trades_10y.csv')
    t['date'] = pd.to_datetime(t.date)
    dtm = t.groupby('date').pnl.sum().resample('ME').sum()
    rows = []
    for d, w, condor in itertools.product([0.05, 0.07], [0.03, 0.05], [False, True]):
        daily, m = run(d, w, condor)
        s = st(daily)
        si, so = st(daily[daily.index <= IS_END]), st(daily[daily.index > IS_END])
        mm = daily.resample('ME').sum()
        mc = np.corrcoef(mm.values, dtm.reindex(mm.index).fillna(0).values)[0, 1]
        worst = m.nsmallest(2, 'pnl')
        lab = f"d{int(d * 100)} w{int(w * 100)} {'コンドル' if condor else 'プット'}"
        print(f"{lab:14s} 年率 {s['annual']:6.1f}%  Sharpe {s['sharpe']:5.2f}  DD {s['dd']:6.1f}%  IS {si['annual']:6.1f}%  "
              f"OOS {so['annual']:6.1f}%  月次相関 {mc:+.2f}  月 {len(m)}  最悪 "
              f"{[(str(r.sq)[:7], round(r.pnl / CAP * 100, 1)) for r in worst.itertuples()]}", flush=True)
        rows.append(dict(label=lab, **s, is_annual=si['annual'], oos_annual=so['annual'], mcorr=mc))
    g = pd.DataFrame(rows)
    g.to_csv('out/opt_spread.csv', index=False)
    mid = g.iloc[(g.sharpe - g.sharpe.median()).abs().argsort()[:2]]
    c1 = g.sharpe.median() >= 0.6
    c2 = -g.dd.median() <= 40
    c3 = bool((mid.is_annual > 0).all() and (mid.oos_annual > 0).all())
    c4 = g.mcorr.median() <= 0.1
    print(f"\n中央値: Sharpe {g.sharpe.median():.2f}  DD {g.dd.median():.1f}%  月次相関 {g.mcorr.median():+.2f}  中央の格子 {list(mid.label)}")
    print(f"判定: 1 {c1}  2 {c2}  3 {c3}  4 {c4}  → {'採用候補' if c1 and c2 and c3 and c4 else '不採用'}")


if __name__ == '__main__':
    main()
