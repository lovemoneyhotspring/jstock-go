"""市場区分の昇格（→ 東証一部 / プライム）を、TDnet の発表日から買う。

根拠: vault 20-research/2026-09-jp-mkt-promotion.md
（前段 vault 20-research/2026-09-jp-index-rebalance.md の N4。実施日の −5 日の跳ねを発表と推定し、
  −4 日の寄り → −1 日の寄りで費用込み +138.8 bp（t 2.68）だったが、中央値 −14.8 bp・勝率 48.9% の裾駆動で、
  発表日を持たず「実施日を後から知っている」下駄を履いていた。TDnet で発表日を得て測り直す）
事前登録の本体はこの docstring と tdnet_events.py の規則。回す前に commit する。結果を見てから基準・形を変えない。

  bash test/heavy.sh test/.venv/bin/python test/mkt_promotion.py > test/out/mkt_promotion.txt 2>&1

■ 事象
  tdnet_events.events()['promo']: 市場第一部・プライム市場への指定・市場変更の知らせ（申請の段階は含めない。
  同じ銘柄で 60 日以内の重複は最初の 1 件）。
  実施日: master の Mkt が 東証一部（0101）/ プライム（0111）以外 → そのどちらかに変わった日のうち、
  発表の後 60 暦日以内で最初のもの（n4b_promotion.py と同じ検出）。見つからない事象は外し、件数を出す。
  普通株（ProdCat='011'）だけ。期間は 2016-09 〜 2026-09。
■ 入口の日
  発表が 9:00 より前 → その日の寄り。9:00 以降 → 翌営業日の寄り（場中の発表も翌寄り）。
■ 形
  E1（本命）: 入口の寄りで買い → 実施日の前営業日の寄りで売る（N4 の −4 → −1 と同じ出口）。
  入口が実施日の前営業日以降になる事象は E1 から外す。
■ 指標
  超過 = 銘柄の窓のリターン − TOPIX の同じ窓のリターン（寄り → 寄り）。
  純損益 = 超過 − （dt_wf_target.liq_cost_bp（発表日の売買代金 20 日中央値）＋滑り 10 bp）。
  検定は入口の日で束ね（同じ日に複数あれば平均）、日の値で t を取る。
■ 採否（E1）
  1: IS（入口 〜2022-12）・OOS（2023-01〜）とも日の平均 > 0
  2: 日で束ねた t ≥ 2.0
  3: 事象の純損益の中央値 > 0（N4 で欠いた点。裾だけで勝っていないか）
  4: 事象のある暦年（2017〜2025）の 3 分の 2 以上で、年の平均 > 0
  すべて満たせば採用候補とし、籠（同時に持つ数・資金配分）を別に事前登録する。
■ 報告だけ
  発表の反応（前日の引け → 入口の寄り）、発表から実施日までの営業日数の分布、
  E2 = 実施日の前営業日の寄り → 実施日から 10 営業日後の寄り（組み入れ後の反落）、時価総額の 3 分位ごとの E1。
"""
import sys

sys.path.insert(0, __import__('os').path.dirname(__file__))
from common import *  # noqa: E402,F403
from dt_wf_target import liq_cost_bp  # noqa: E402
import tdnet_events as te  # noqa: E402

SLIP_BP = 10.0
PRIME = {'0101', '0111'}
E2_DAYS = 10


def main():
    c = con()
    tp = topix(c)
    tdays = tp.index
    px = c.execute("SELECT Date, Code, AdjO, AdjC, Va, MktCap FROM bars WHERE AdjC>0 AND AdjO>0").df()
    px['Date'] = pd.to_datetime(px.Date)
    px = px[px.Date.isin(tdays)]
    W = {k: px.pivot(index='Date', columns='Code', values=k).reindex(tdays) for k in ['AdjO', 'AdjC', 'Va', 'MktCap']}
    va = W['Va'].rolling(20, min_periods=10).median()
    m = c.execute("SELECT Date, Code, Mkt FROM master WHERE ProdCat='011' AND Mkt<>'0105'").df()
    m['Date'] = pd.to_datetime(m.Date)
    m = m.sort_values(['Code', 'Date'])
    m['pMkt'] = m.groupby('Code').Mkt.shift(1)
    up = m[(m.Mkt != m.pMkt) & m.pMkt.notna() & m.Mkt.isin(PRIME) & ~m.pMkt.isin(PRIME)]
    ev = te.events()['promo']

    def open_pos(ts):
        d = pd.Timestamp(ts.date())
        return int(np.searchsorted(tdays, d, side='left' if ts.hour < 9 else 'right'))

    rows, miss = [], 0
    for _, e in ev.iterrows():
        code, ts = e.code, e.pubdate
        if ts < tdays[0] + pd.Timedelta(days=30):
            continue
        u = up[(up.Code == code) & (up.Date > ts) & (up.Date <= ts + pd.Timedelta(days=60))]
        if not len(u) or code not in W['AdjO'].columns:
            miss += 1
            continue
        eff = int(np.searchsorted(tdays, u.Date.iloc[0], side='left'))
        a = open_pos(ts)
        b = eff - 1
        O = W['AdjO'][code].values
        C = W['AdjC'][code].values
        r = dict(code=code, ann=ts, entry=tdays[a] if a < len(tdays) else pd.NaT, eff=tdays[eff] if eff < len(tdays) else pd.NaT,
                 lag=eff - a, mcap=W['MktCap'][code].values[a - 1])
        cost = (float(liq_cost_bp(np.nan_to_num(va[code].values[a - 1]))) + SLIP_BP) / 1e4
        if a < len(tdays) and np.isfinite(O[a]) and np.isfinite(C[a - 1]):
            r['react_ex'] = (O[a] / C[a - 1] - 1) - (tp.open.iloc[a] / tp.close.iloc[a - 1] - 1)
        if a < b < len(tdays) and np.isfinite(O[a]) and np.isfinite(O[b]):
            ex = (O[b] / O[a] - 1) - (tp.open.iloc[b] / tp.open.iloc[a] - 1)
            r['e1_ex'], r['e1'] = ex, ex - cost
        z = eff + E2_DAYS
        if z < len(tdays) and b >= 0 and np.isfinite(O[b]) and np.isfinite(O[z]):
            ex = (O[z] / O[b] - 1) - (tp.open.iloc[z] / tp.open.iloc[b] - 1)
            r['e2_ex'], r['e2'] = ex, ex - cost
        rows.append(r)
    df = pd.DataFrame(rows)
    df.to_csv(f'{OUT}/mkt_promotion.csv', index=False)
    pd.set_option('display.width', 200)
    print(f'昇格の知らせ {len(ev)} 件、実施日が見つかった {len(df)}、見つからない {miss}')
    print('年ごとの件数:', df.groupby(pd.to_datetime(df.entry).dt.year).size().to_dict())
    print('入口から実施日までの営業日数:', df.lag.value_counts().sort_index().head(15).to_dict())
    print(f'発表の反応（前日の引け→入口の寄り）の平均 {df.react_ex.mean() * 1e4:+.1f} bp・中央値 {df.react_ex.median() * 1e4:+.1f}')

    def judge(w, name, decide):
        d = df.dropna(subset=[w]).copy()
        d['day'] = pd.to_datetime(d.entry)
        day = d.groupby('day')[w].mean() * 1e4
        isx, oos = day[day.index <= IS_END], day[day.index > IS_END]
        t = day.mean() / day.std(ddof=1) * np.sqrt(len(day))
        med = d[w].median() * 1e4
        yr = d[(d.day.dt.year >= 2017) & (d.day.dt.year <= 2025)].groupby(d.day.dt.year)[w].mean() * 1e4
        print(f'\n=== {name}（{len(d)} 件・{len(day)} 日）')
        print(f'日の平均 {day.mean():+.1f} bp  t {t:+.2f}  IS {isx.mean():+.1f} ({len(isx)})  OOS {oos.mean():+.1f} ({len(oos)})  '
              f'事象の平均 {d[w].mean() * 1e4:+.1f}・中央値 {med:+.1f}・勝率 {(d[w] > 0).mean():.0%}  超過（コスト前）{d[w + "_ex"].mean() * 1e4:+.1f}')
        print('年の平均:', yr.round(1).to_dict(), ' 年の件数:', d.groupby(d.day.dt.year).size().to_dict())
        d['q'] = pd.qcut(d.mcap.rank(method='first'), 3, labels=['小', '中', '大'])
        print('時価総額の 3 分位:', (d.groupby('q', observed=True)[w].mean() * 1e4).round(1).to_dict())
        if decide:
            need = int(np.ceil(len(yr) * 2 / 3))
            c1, c2, c3, c4 = isx.mean() > 0 and oos.mean() > 0, t >= 2.0, med > 0, int((yr > 0).sum()) >= need
            print(f'判定 E1: 1 {c1}  2 {c2}  3 {c3}  4 {c4}（{int((yr > 0).sum())}/{len(yr)} 年、要 {need}）  → '
                  f'{"採用候補" if c1 and c2 and c3 and c4 else "不採用"}')

    judge('e1', 'E1 発表後の寄りで買い → 実施日の前営業日の寄り', True)
    judge('e2', '（報告）E2 実施日の前営業日の寄り → 実施日から 10 営業日後の寄り', False)


if __name__ == '__main__':
    main()
