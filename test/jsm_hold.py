"""JPX日経中小型株指数の構成銘柄を買い持ちすると、規模の近い非構成銘柄・TOPIX に勝つか。

根拠: vault 20-research/2026-09-jp-jsm-hold.md
（中小型株指数は ROE・営業利益・ガバナンスで選んだ「質の良い小型株」の一覧。一覧そのものに超過があるか）
事前登録の本体はこの docstring。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/jsm_hold.py > test/out/jsm_hold.txt 2>&1

■ 持ち方
  月の初めの営業日の時点で有効な構成銘柄（test/data/jpx_index_members.csv の jsm。定期入替の実施日から次の実施日の前日まで）を、
  前月末の引けで等金額に買い、その月末の引けまで持つ（毎月等金額に戻す）。期間は 2017-09 〜 2026-08。
  月の途中で値が無くなった銘柄（上場廃止・TOB など）は、最後にある引けで返したとする（消えた銘柄を落とさない）。
■ 比べる相手（毎月作り直す）
  C1 規模の対照: 普通株（ProdCat='011'）で、JPX日経中小型・JPX日経400 のどちらの構成銘柄でもなく、
     前月末の時価総額が構成銘柄の 10〜90% の範囲、売買代金 20 日中央値が構成銘柄の 10% 点以上。等金額。
  C2 TOPIX（配当なし指数。個別株の調整後の値も配当を含まないので揃っている）。
■ 採否
  主の差 = 構成銘柄 − C1 の月次の差。
  1: 差の平均 > 0、月の t ≥ 2.0
  2: IS（〜2022-12）・OOS（2023-01〜）とも差の平均 > 0
  3: 指数の年（9 月〜翌 8 月の 9 年）で 6 年以上、差の年の合計 > 0
  すべて満たせば、実運用の形（ヘッジの方法・銘柄数を絞るか）を事前登録する。
  売買の費用は、年 1 回の入替（200 銘柄中 45〜55 銘柄）と毎月の等金額への戻しで年 0.2〜0.4% の桁と見込み、報告で差し引いて見せる。
■ 報告だけ
  構成銘柄 − TOPIX、年ごとの表、構成銘柄の中で「入ったばかり（1 年目）」と「2 年以上いる」の差、
  月ごとの横断回帰（リターン ~ 構成銘柄 + log 時価総額 + log 売買代金 + 直近 12-1 か月の騰落）の構成銘柄の係数。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403

START, END = pd.Timestamp('2017-09-01'), pd.Timestamp('2026-08-31')
COST_YR = 0.003   # 報告で差し引く年の売買費用（見込み）


def tstat(x):
    x = pd.Series(x).dropna()
    return x.mean() / x.std(ddof=1) * np.sqrt(len(x))


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjC, Va, MktCap FROM bars WHERE AdjC>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    C = px.pivot(index='Date', columns='Code', values='AdjC').reindex(tdays)
    MC = px.pivot(index='Date', columns='Code', values='MktCap').reindex(tdays)
    VA = px.pivot(index='Date', columns='Code', values='Va').reindex(tdays).rolling(20, min_periods=10).median()
    ms = c.execute("SELECT Date, Code, ProdCat FROM master").df()
    ms['Date'] = pd.to_datetime(ms.Date)
    mem = pd.read_csv(f'{OUT}/../data/jpx_index_members.csv', dtype=str)
    mem['start'], mem['end'] = pd.to_datetime(mem.start), pd.to_datetime(mem.end)

    months = pd.period_range(START, END, freq='M')
    ends = {p: tdays[tdays.to_period('M') == p][-1] for p in months if (tdays.to_period('M') == p).any()}
    rows, xs = [], []
    for p in months:
        e = ends[p]
        s = tdays[tdays < tdays[tdays.to_period('M') == p][0]][-1]   # 前月末
        first = tdays[tdays.to_period('M') == p][0]
        act = mem[(mem.start <= first) & (mem.end >= first)]
        jsm = {k + '0' for k in act[act['index'] == 'jsm'].code}
        j400 = {k + '0' for k in act[act['index'] == 'j400'].code}
        c0 = C.loc[s]
        win = C.loc[s:e]
        c1 = win.ffill().loc[e]                                  # 月の途中で消えた銘柄は最後の引け
        r = (c1 / c0 - 1)[c0.notna()]
        mc, va = MC.loc[s], VA.loc[s]
        common = set(ms[(ms.Date <= s) & (ms.Date > s - pd.Timedelta(days=10)) & (ms.ProdCat == '011')].Code)
        M = [k for k in jsm if k in r.index and np.isfinite(r[k])]
        lo, hi, vlo = mc[M].quantile(.1), mc[M].quantile(.9), va[M].quantile(.1)
        ctl = r.index[mc.reindex(r.index).between(lo, hi) & (va.reindex(r.index) >= vlo)
                      & ~r.index.isin(jsm | j400) & r.index.isin(common)]
        # 入ったばかり（直近の定期入替で入った）か
        cur_start = act[act['index'] == 'jsm'].start.max()
        prev = mem[(mem['index'] == 'jsm') & (mem.end == cur_start - pd.Timedelta(days=1))]
        prevset = {k + '0' for k in prev.code}
        new = [k for k in M if k not in prevset] if len(prev) else []
        old = [k for k in M if k in prevset] if len(prev) else []
        rows.append(dict(m=str(p), n=len(M), n_ctl=len(ctl), mem=r[M].mean(), ctl=r[ctl].mean(),
                         tpx=tp.close[e] / tp.close[s] - 1,
                         new=r[new].mean() if new else np.nan, old=r[old].mean() if old else np.nan))
        # 横断回帰の材料
        i12 = int(np.searchsorted(tdays, s)) - 250
        i1 = int(np.searchsorted(tdays, s)) - 21
        if i12 >= 0:
            U = list(M) + list(ctl)
            X = pd.DataFrame({'r': r.reindex(U), 'mem': [1.0] * len(M) + [0.0] * len(ctl),
                              'lmc': np.log(mc.reindex(U)), 'lva': np.log(va.reindex(U)),
                              'mom': (C.iloc[i1].reindex(U) / C.iloc[i12].reindex(U) - 1)}).dropna()
            A = np.column_stack([np.ones(len(X)), X[['mem', 'lmc', 'lva', 'mom']].values])
            b, *_ = np.linalg.lstsq(A, X.r.values, rcond=None)
            xs.append(dict(m=str(p), coef=b[1]))
    df = pd.DataFrame(rows).set_index('m')
    df.to_csv(f'{OUT}/jsm_hold.csv')
    df['diff'] = df.mem - df.ctl
    df['vs_tpx'] = df.mem - df.tpx
    pd.set_option('display.width', 200)
    idx = pd.PeriodIndex(df.index, freq='M')
    isx, oos = df['diff'][idx.to_timestamp() <= IS_END], df['diff'][idx.to_timestamp() > IS_END]
    fy = df['diff'].groupby([(p.year if p.month >= 9 else p.year - 1) for p in idx]).sum()
    t = tstat(df['diff'])
    print(f'{len(df)} か月  構成銘柄 平均 {df.n.mean():.0f}・対照 平均 {df.n_ctl.mean():.0f}')
    print(f'\n=== 主: 構成銘柄 − 規模の対照（月次）')
    print(f'月の平均 {df["diff"].mean() * 1e4:+.1f} bp（年率 {df["diff"].mean() * 12:+.2%}）  t {t:+.2f}  '
          f'IS {isx.mean() * 1e4:+.1f}  OOS {oos.mean() * 1e4:+.1f}  正の月 {(df["diff"] > 0).mean():.0%}')
    print('指数の年（9 月〜）ごとの合計:', (fy * 100).round(2).to_dict())
    c1, c2, c3 = df['diff'].mean() > 0 and t >= 2.0, isx.mean() > 0 and oos.mean() > 0, int((fy > 0).sum()) >= 6
    print(f'判定: 1 {c1}  2 {c2}  3 {c3}（{int((fy > 0).sum())}/{len(fy)} 年）  → {"採用候補" if c1 and c2 and c3 else "不採用"}')
    print(f'（報告）売買費用 年 {COST_YR:.1%} を引いた年率 {df["diff"].mean() * 12 - COST_YR:+.2%}')
    print(f'\n（報告）構成銘柄 − TOPIX: 月 {df.vs_tpx.mean() * 1e4:+.1f} bp（年率 {df.vs_tpx.mean() * 12:+.2%}、t {tstat(df.vs_tpx):+.2f}）  '
          f'構成銘柄の年率 {df.mem.mean() * 12:+.2%}  対照 {df.ctl.mean() * 12:+.2%}  TOPIX {df.tpx.mean() * 12:+.2%}')
    nd = (df.new - df.ctl).dropna()
    od = (df.old - df.ctl).dropna()
    print(f'（報告）入ったばかり − 対照: 月 {nd.mean() * 1e4:+.1f} bp（t {tstat(nd):+.2f}）  2 年以上 − 対照: {od.mean() * 1e4:+.1f} bp（t {tstat(od):+.2f}）')
    x = pd.DataFrame(xs).set_index('m').coef
    print(f'（報告）横断回帰の構成銘柄の係数: 月 {x.mean() * 1e4:+.1f} bp（t {tstat(x):+.2f}、{len(x)} か月）')
    print('\n年ごとの年率（構成銘柄・対照・TOPIX、%）:')
    g = df.groupby([(p.year if p.month >= 9 else p.year - 1) for p in idx])[['mem', 'ctl', 'tpx', 'diff']].sum() * 100
    print(g.round(1).to_string())


if __name__ == '__main__':
    main()
