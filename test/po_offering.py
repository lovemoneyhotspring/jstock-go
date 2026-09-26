"""公募増資（売出しを伴う新株発行・自己株式の処分）の発表から条件決定までの売り、条件決定後の戻りの買い。

根拠: vault 20-research/2026-09-jp-public-offering.md
（「決まった日に値段を気にせず売買する人がいる」型の続き。発表日は TDnet の表題から拾う: tdnet_fetch.py・tdnet_events.py）
事前登録の本体はこの docstring と tdnet_events.py の規則。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/po_offering.py > test/out/po_offering.txt 2>&1

■ 事象
  tdnet_events.events()['po']: 公募増資の発表（同じ銘柄で 30 日以内の続報は最初の 1 件だけ）。
  条件決定日: 同じ銘柄の「発行価格・売出価格・処分価格 … 決定」の知らせのうち、発表の後 2〜30 暦日で最初のもの。
  条件決定の知らせが無い事象は P1・P2 から外し、件数を出す（中止・延期を含む）。
  期間: 調整後の株価がある 2016-09 〜 2026-09。普通株（master の ProdCat='011'）だけ。
■ 入口の日（発表時刻で決める）
  発表が 9:00 より前 → その日の寄り。9:00 以降 → 翌営業日の寄り（場中の発表も翌寄り。保守側）。
  条件決定の知らせも同じ規則で「条件決定の後の最初の寄り」を決める。
■ 形
  P1（売り）: 発表後の最初の寄りで売り → 条件決定の知らせが出た日の引けで買い戻す
             （条件決定の知らせが引け後なら、その日の引け。9:00 より前なら前営業日の引け）。
  P2（買い）: 条件決定の後の最初の寄りで買い → 5 営業日後の引けで売る。
■ 指標
  超過 = 銘柄の窓のリターン − TOPIX の同じ窓のリターン（寄り・引けも合わせる）。
  純損益 = 向き × 超過 − （dt_wf_target.liq_cost_bp（発表日の売買代金 20 日中央値）＋滑り 10 bp）
          − 売りだけ 年 3.1% × 保有営業日/245（逆日歩は入れない）。
  検定は入口の日で束ね（同じ日に複数あれば平均）、日の値で t を取る。
■ 採否（P1・P2 それぞれ）
  1: IS（入口 〜2022-12）・OOS（2023-01〜）とも日の平均 > 0
  2: 日で束ねた t ≥ 2.0
  3: 事象の純損益の中央値 > 0（裾だけで勝っていないか）
  4: 暦年（2017〜2025 の 9 年）で 6 年以上、年の平均 > 0
  すべて満たせば採用候補。P1 は日証金の貸株申込停止・逆日歩で建てられない/高くつく銘柄が多いと見込まれるので、
  次に「一般信用で売れる銘柄か」を前向きに確かめてから実運用の形を事前登録する。
■ 報告だけ
  発表の反応（発表前の引け → 入口の寄り）、発表前 20 営業日の超過、条件決定までの営業日数、
  貸借銘柄の割合、時価総額で 3 分位に分けた P1・P2、株式の売出しだけの事象（tdnet_events の 'so'）の P1・P2 相当。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from dt_wf_target import liq_cost_bp  # noqa: E402
import tdnet_events as te  # noqa: E402

SLIP_BP = 10.0
BORROW = 0.031
P2_DAYS = 5
P0_DAYS = 20


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va', 'MktCap']}
    va = W['Va'].rolling(20, min_periods=10).median()
    ms = c.execute("SELECT Date, Code, ProdCat, Mrgn FROM master").df()
    ms['Date'] = pd.to_datetime(ms.Date)
    ms = ms.sort_values('Date')
    ev = te.events()
    price = ev['price']

    def open_pos(ts):
        """発表時刻から入口の寄りの位置。"""
        d = pd.Timestamp(ts.date())
        if ts.hour < 9:
            return int(np.searchsorted(tdays, d, side='left'))
        return int(np.searchsorted(tdays, d, side='right'))

    def close_pos(ts):
        """知らせの時刻から、それを知る前の最後の引けの位置（引け後ならその日、9:00 前なら前日）。"""
        d = pd.Timestamp(ts.date())
        if ts.hour < 9:
            return int(np.searchsorted(tdays, d, side='left')) - 1
        return int(np.searchsorted(tdays, d, side='right')) - 1

    def info(code, d):
        s = ms[(ms.Code == code) & (ms.Date <= d)]
        return (str(s.ProdCat.iloc[-1]), str(s.Mrgn.iloc[-1])) if len(s) else (None, None)

    rows = []
    for kind in ('po', 'so'):
        for _, e in ev[kind].iterrows():
            code, ts = e.code, e.pubdate
            if ts < tdays[0] + pd.Timedelta(days=40) or code not in W['AdjC'].columns:
                continue
            a = open_pos(ts)
            if a >= len(tdays):
                continue
            prod, mrgn = info(code, tdays[a])
            if prod != '011':
                continue
            O, C = W['AdjO'][code].values, W['AdjC'][code].values
            a0 = a - 1
            r = dict(kind=kind, code=code, ann=ts, entry=tdays[a], shortable=mrgn == '2',
                     mcap=W['MktCap'][code].values[a0])
            cost = (float(liq_cost_bp(np.nan_to_num(va[code].values[a0]))) + SLIP_BP) / 1e4
            if np.isfinite(O[a]) and np.isfinite(C[a0]):
                r['react_ex'] = (O[a] / C[a0] - 1) - (tp.open.iloc[a] / tp.close.iloc[a0] - 1)
            if a0 - P0_DAYS >= 0 and np.isfinite(C[a0 - P0_DAYS]):
                r['p0_ex'] = (C[a0] / C[a0 - P0_DAYS] - 1) - (tp.close.iloc[a0] / tp.close.iloc[a0 - P0_DAYS] - 1)
            pr = price[(price.code == code) & (price.pubdate > ts + pd.Timedelta(days=2))
                       & (price.pubdate <= ts + pd.Timedelta(days=30))]
            if len(pr):
                pts = pr.pubdate.iloc[0]
                b = close_pos(pts)
                q = open_pos(pts)
                r['price_at'] = pts
                r['days_to_price'] = b - a + 1
                if b >= a and b < len(tdays) and np.isfinite(O[a]) and np.isfinite(C[b]):
                    ex = (C[b] / O[a] - 1) - (tp.close.iloc[b] / tp.open.iloc[a] - 1)
                    r['p1_ex'] = ex
                    r['p1'] = -ex - cost - BORROW * (b - a + 1) / 245
                z = q + P2_DAYS - 1
                if z < len(tdays) and np.isfinite(O[q]) and np.isfinite(C[z]):
                    ex = (C[z] / O[q] - 1) - (tp.close.iloc[z] / tp.open.iloc[q] - 1)
                    r['p2_ex'] = ex
                    r['p2'] = ex - cost
                    r['p2_entry'] = tdays[q]
            rows.append(r)
    df = pd.DataFrame(rows)
    df.to_csv(f'{OUT}/po_offering.csv', index=False)
    pd.set_option('display.width', 200)
    po = df[df.kind == 'po']
    print(f'公募増資 {len(po)} 件（条件決定あり {po.price_at.notna().sum()}、P1 を測れた {po.p1.notna().sum()}、'
          f'P2 {po.p2.notna().sum()}）、貸借銘柄 {po.shortable.mean():.0%}')
    print('条件決定までの営業日数:', po.days_to_price.describe().round(1).to_dict())
    print(f'発表の反応（前日の引け→入口の寄り）の平均 {po.react_ex.mean() * 1e4:+.1f} bp・中央値 {po.react_ex.median() * 1e4:+.1f}  '
          f'発表前 20 日 {po.p0_ex.mean() * 1e4:+.1f} bp')

    def judge(d, w, entry, name, decide=True):
        d = d.dropna(subset=[w]).copy()
        d['day'] = pd.to_datetime(d[entry])
        day = d.groupby('day')[w].mean() * 1e4
        isx, oos = day[day.index <= IS_END], day[day.index > IS_END]
        t = day.mean() / day.std(ddof=1) * np.sqrt(len(day))
        med = d[w].median() * 1e4
        yr = d[(d.day.dt.year >= 2017) & (d.day.dt.year <= 2025)].groupby(d.day.dt.year)[w].mean() * 1e4
        print(f'\n=== {name}（{len(d)} 件・{len(day)} 日）')
        print(f'日の平均 {day.mean():+.1f} bp  t {t:+.2f}  IS {isx.mean():+.1f} ({len(isx)})  OOS {oos.mean():+.1f} ({len(oos)})  '
              f'事象の平均 {d[w].mean() * 1e4:+.1f}・中央値 {med:+.1f}・勝率 {(d[w] > 0).mean():.0%}  超過（コスト前）{d[w + "_ex"].mean() * 1e4:+.1f}')
        print('年の平均:', yr.round(1).to_dict())
        d['q'] = pd.qcut(d.mcap.rank(method='first'), 3, labels=['小', '中', '大'])
        print('時価総額の 3 分位:', (d.groupby('q', observed=True)[w].mean() * 1e4).round(1).to_dict())
        if decide:
            c1, c2, c3, c4 = isx.mean() > 0 and oos.mean() > 0, t >= 2.0, med > 0, int((yr > 0).sum()) >= 6
            print(f'判定: 1 {c1}  2 {c2}  3 {c3}  4 {c4}（{int((yr > 0).sum())}/{len(yr)} 年）  → '
                  f'{"採用候補" if c1 and c2 and c3 and c4 else "不採用"}')

    judge(po, 'p1', 'entry', 'P1 発表後の寄りで売り → 条件決定の日の引けで買い戻す')
    judge(po, 'p2', 'p2_entry', 'P2 条件決定後の寄りで買い → 5 営業日後の引け')
    so = df[df.kind == 'so']
    print(f'\n（報告）株式の売出しだけ {len(so)} 件')
    judge(so, 'p1', 'entry', '（報告）売出しだけの P1', decide=False)
    judge(so, 'p2', 'p2_entry', '（報告）売出しだけの P2', decide=False)


if __name__ == '__main__':
    main()
