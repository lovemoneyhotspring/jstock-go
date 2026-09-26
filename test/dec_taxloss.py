"""12 月の損出し売り: 年初来の負け銘柄は 12 月（年内受渡しの最終売買日まで）に母集団より下げ、年明けに戻るか。

根拠: vault 20-research/2026-09-jp-dec-taxloss.md（daytrade の 12 月休みの理由 = 12 月だけ下げた銘柄がさらに下げる）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/dec_taxloss.py > test/out/dec_taxloss.txt 2>&1

■ 母集団（信号日の時点）
  月末の master で内国株（ProdCat=011、TOKYO PRO 除く、S33≠9999）かつ貸借銘柄（Mrgn=2）、
  信号日の売買代金 20 日中央値 ≥ 1 億円。
■ 信号
  信号日 = 11 月の最終営業日。年初来 = 前年の最終営業日の終値 → 信号日の終値（調整済み）。
  負け = 母集団の年初来の下位 10%、勝ち = 上位 10%（勝ちは報告だけ）。
■ 窓
  最終売買日 L = その年の最終営業日から受渡しの日数（2019-07-16 より前は 3、以後は 2）だけ前の営業日。
  S1（順張りショート）: 12 月の最初の営業日の寄りで負けを売り、L の引けで買い戻す。
  S2（年明けの戻りのロング）: L の翌営業日の寄りで負けを買い、翌年 1 月の 10 営業日目の寄りで返す。
  上場廃止などで返しの値が無い銘柄は最後の値で返す。
■ 指標
  超過 = 銘柄の窓のリターン − 同じ窓の母集団の等金額平均。1 年 1 窓で、負けの等金額平均を年の値とする。
  S1 の純損益 = −超過 − （dt_wf_target.liq_cost_bp ＋ 滑り 10 bp）− 年 3.1% × 保有営業日/245
  S2 の純損益 = 超過 − （liq_cost_bp ＋ 滑り 10 bp）
  年は 2017〜2025 の 9 回（S2 は 2018〜2026 の 1 月）。
■ 対照（12 月に特有か）
  2〜11 月の各月で同じ形を作る（信号 = 前月末、年初来は同じ定義、窓 = 月の最初の寄り → 月末から同じ日数だけ前の引け、
  S2 は その翌営業日の寄り → 翌月の 10 営業日目の寄り）。年ごとに「12 月 − 同じ年の対照の平均」を取る。
■ 採否（S1・S2 それぞれ）
  1: 純損益が 9 年中 7 年以上で正
  2: 9 年の純損益の平均 > 0 かつ年の t ≥ 2.0
  3: 年ごとの「12 月 − 対照」の超過の差が、S1 は負・S2 は正で、その t の絶対値 ≥ 2.0
  すべて満たせば採用候補（年 1 回の季節の上乗せとして、資金・DD を別の事前登録で測る）。満たさなければ不採用。
■ 報告だけ
  勝ちの同じ表、12 月の日ごとの累積超過（負け）、年ごとの値。
"""

import sys
sys.path.insert(0, __file__.rsplit('/', 1)[0])
from common import *  # noqa: E402,F403

from dt_wf_target import liq_cost_bp  # noqa: E402

Q = 0.10
MIN_VA = 1e8
SLIP_BP = 10.0
BORROW = 0.031
T2_FROM = pd.Timestamp('2019-07-16')
YEARS = range(2017, 2026)


def load():
    c = con()
    tdays = topix(c).index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va']}
    codes = W['AdjO'].columns
    master = c.execute("SELECT Date, Code, Mrgn FROM master WHERE ProdCat='011' AND Mkt<>'0105' AND S33<>'9999'").df()
    master['Date'] = pd.to_datetime(master.Date)
    msnap = {d: set(g.Code[g.Mrgn.astype(str) == '2']) for d, g in master.groupby('Date')}
    return tdays, W, codes, msnap


def main():
    tdays, W, codes, msnap = load()
    mdays = sorted(msnap)
    O, C = W['AdjO'], W['AdjC']
    Of, Cf = O.ffill(limit=30), C.ffill(limit=30)
    va = W['Va'].rolling(20, min_periods=15).median()
    ti = lambda d: int(np.searchsorted(tdays, d))  # noqa: E731  d 以後の最初の営業日の位置

    def month_days(y, m):
        return np.nonzero((tdays.year == y) & (tdays.month == m))[0]

    def event(y, m):
        """y 年 m 月の 1 窓。戻り値 {'S1': (超過, 純), 'S2': (...), 'W1': ..., 'W2': ...}"""
        md = month_days(y, m)
        prev = month_days(y, m - 1) if m > 1 else month_days(y - 1, 12)
        py = np.nonzero(tdays.year == y - 1)[0]
        if len(md) < 10 or len(prev) == 0 or len(py) == 0:
            return None
        sig, y0 = prev[-1], py[-1]
        lag = 3 if tdays[md[-1]] < T2_FROM else 2
        L = md[-1] - lag
        nm = month_days(y + (m == 12), m % 12 + 1)
        if len(nm) < 10:
            return None
        s2_in, s2_out = L + 1, nm[9]
        snap = [d for d in mdays if d <= tdays[sig]]
        if not snap:
            return None
        ok = codes.isin(msnap[snap[-1]]) & (va.values[sig] >= MIN_VA) & np.isfinite(C.values[y0]) & np.isfinite(C.values[sig])
        ytd = C.values[sig] / C.values[y0] - 1
        idx = np.nonzero(ok)[0]
        r = pd.Series(ytd[idx], index=idx)
        lo = r[r <= r.quantile(Q)].index.values
        hi = r[r >= r.quantile(1 - Q)].index.values
        e1, x1 = md[0], L
        R1 = Cf.values[x1] / O.values[e1] - 1
        R2 = Of.values[s2_out] / O.values[s2_in] - 1
        u1, u2 = np.nanmean(R1[idx]), np.nanmean(R2[idx])
        cost = (liq_cost_bp(va.values[sig]) + SLIP_BP) / 1e4
        out = {}
        for nm_, grp in (('L', lo), ('W', hi)):
            ex1 = np.nanmean(R1[grp] - u1)
            ex2 = np.nanmean(R2[grp] - u2)
            c = np.nanmean(cost[grp])
            out[nm_ + '1'] = (ex1, -ex1 - c - BORROW * (x1 - e1 + 1) / 245)
            out[nm_ + '2'] = (ex2, ex2 - c)
        # 12 月の日ごとの累積超過（負け、終値）
        path = Cf.values[e1 - 1:x1 + 1]
        cum = np.nanmean(path[:, lo] / path[0, lo], axis=1) - np.nanmean(path[:, idx] / path[0, idx], axis=1)
        out['path'] = cum
        return out

    rows, paths = [], []
    for y in YEARS:
        for m in range(2, 13):
            e = event(y, m)
            if e is None:
                continue
            if m == 12:
                paths.append(pd.Series(e.pop('path'), name=y))
            else:
                e.pop('path')
            for k, (ex, net) in e.items():
                rows.append(dict(y=y, m=m, leg=k, ex=ex * 1e4, net=net * 1e4))
    df = pd.DataFrame(rows)
    df.to_csv(f'{OUT}/dec_taxloss.csv', index=False)

    def tt(x):
        x = np.asarray(x, float)
        return x.mean() / x.std(ddof=1) * np.sqrt(len(x))

    pd.set_option('display.width', 200)
    verdict = {}
    for leg, name in (('L1', 'S1 負けの順張りショート（12 月）'), ('L2', 'S2 負けの年明けロング'),
                      ('W1', '勝ちを売る（報告だけ）'), ('W2', '勝ちの年明けロング（報告だけ）')):
        d = df[df.leg == leg]
        dec = d[d.m == 12].set_index('y')
        plc = d[d.m != 12].groupby('y').ex.mean()
        diff = (dec.ex - plc).dropna()
        print(f'\n=== {name}')
        print(pd.DataFrame({'超過bp': dec.ex, '純bp': dec.net, '対照の超過bp': plc, '差': diff}).round(1).T.to_string())
        n_pos = int((dec.net > 0).sum())
        print(f'純: 平均 {dec.net.mean():+.1f} bp  t {tt(dec.net):+.2f}  正の年 {n_pos}/{len(dec)}   '
              f'12 月 − 対照: 平均 {diff.mean():+.1f} bp  t {tt(diff):+.2f}')
        want = -1 if leg.endswith('1') else 1
        verdict[leg] = (n_pos >= 7, dec.net.mean() > 0 and tt(dec.net) >= 2.0, want * tt(diff) >= 2.0)
    for leg in ('L1', 'L2'):
        v = verdict[leg]
        print(f'判定 {leg}: 1 {v[0]}  2 {v[1]}  3 {v[2]}  → {"採用候補" if all(v) else "不採用"}')
    cv = pd.concat(paths, axis=1)
    print('\n12 月の累積超過（負け − 母集団、bp、年の平均。0 = 11 月末の終値）:')
    print(' '.join(f'{v:+.0f}' for v in cv.mean(axis=1).values * 1e4))


if __name__ == '__main__':
    main()
