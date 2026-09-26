"""公募増資の P1 を前向きに記録する（毎営業日 19:45、deploy/po-forward.sh から）。

根拠: vault 20-research/2026-09-jp-public-offering.md。紙の上の記録だけで、発注はしない。

  test/.venv/bin/python test/po_forward.py [--vault ~/obsidian-vault]

1. 直近 5 日の TDnet を取り直し（今日と前日は必ず取り直す）、tdnet_events の規則で公募増資・条件決定を拾う。
2. 発表から 45 日以内の事象ごとに、入口の日（po_offering と同じ規則）から、条件決定の日または今日までの
   各営業日の日証金の状態（jsf_lending.fetch: 申込停止・注意喚起・逆日歩）を引く。
3. 株価が入っていれば P1（入口の寄りで売り → 条件決定の日の引けで買い戻す、TOPIX 超過）と、
   逆日歩・貸株料 年 3.1%・流動性別コスト＋滑り 10 bp を引いた実額を出す（po_jsf_check と同じ計算）。
4. 状態を test/out/po_forward/state.csv に、一覧を vault の 20-research/2026-09-jp-public-offering-forward.md に書く。
"""
import argparse
import json
import os
import sys
from datetime import date, timedelta

sys.path.insert(0, os.path.dirname(__file__))
from common import *  # noqa: E402,F403
import tdnet_events as te  # noqa: E402
from tdnet_fetch import DIR as TDIR, fetch as tfetch  # noqa: E402
from jsf_lending import fetch as jfetch, DIR as JDIR  # noqa: E402
from dt_wf_target import liq_cost_bp  # noqa: E402

STATE = os.environ.get('PO_FWD_STATE', os.path.join(OUT, 'po_forward'))
NOTE = '20-research/2026-09-jp-public-offering-forward.md'
BORROW, SLIP_BP = 0.031, 10.0
START = pd.Timestamp(os.environ.get('PO_FWD_START', '2026-09-26'))   # これより後の発表だけを前向きの標本にする（試運転で差し替える）


def recent_tdnet(days=45):
    today = date.today()
    os.makedirs(TDIR, exist_ok=True)
    for i in range(5):
        d = today - timedelta(days=i)
        p = os.path.join(TDIR, d.strftime('%Y%m%d') + '.json')
        if i < 2 and os.path.exists(p):
            os.remove(p)
        tfetch(pd.Timestamp(d))
    rows = []
    for i in range(days):
        p = os.path.join(TDIR, (today - timedelta(days=i)).strftime('%Y%m%d') + '.json')
        if os.path.exists(p):
            rows += json.load(open(p))
    df = pd.DataFrame(rows)
    df['pubdate'] = pd.to_datetime(df.pubdate)
    df['code'] = df.company_code.astype(str).str.strip()
    return df[df.code.str.len() == 5]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--vault', default=os.path.expanduser('~/obsidian-vault'))
    args = ap.parse_args()
    os.makedirs(STATE, exist_ok=True)
    c = con()
    cal = c.execute("SELECT Date FROM cal WHERE HolDiv='1' ORDER BY Date").df()
    bdays = pd.DatetimeIndex(pd.to_datetime(cal.Date))
    today = pd.Timestamp(date.today())
    tp = topix(c)

    df = recent_tdnet()
    ev = te.events(df)
    po, price = ev['po'], ev['price']
    po = po[po.pubdate >= START]
    names = df.drop_duplicates('code').set_index('code').company_name

    def open_day(ts):
        d = pd.Timestamp(ts.date())
        return bdays[int(np.searchsorted(bdays, d, side='left' if ts.hour < 9 else 'right'))]

    def close_day(ts):
        d = pd.Timestamp(ts.date())
        return bdays[int(np.searchsorted(bdays, d, side='left' if ts.hour < 9 else 'right')) - 1]

    sp = os.path.join(STATE, 'state.csv')
    old = pd.read_csv(sp, dtype={'code': str}) if os.path.exists(sp) and os.path.getsize(sp) > 1 else pd.DataFrame()
    rows = []
    for _, e in po.iterrows():
        code, ts = e.code, e.pubdate
        entry = open_day(ts)
        pr = price[(price.code == code) & (price.pubdate > ts + pd.Timedelta(days=2)) & (price.pubdate <= ts + pd.Timedelta(days=30))]
        exit_ = close_day(pr.pubdate.iloc[0]) if len(pr) else pd.NaT
        m = c.execute(f"SELECT Mrgn FROM master WHERE Code='{code}' AND Date<='{ts.date()}' ORDER BY Date DESC LIMIT 1").fetchone()
        mrgn = str(m[0]) if m else ''
        r = dict(code=code[:4], name=names.get(code, ''), ann=ts, shortable=mrgn == '2', entry=entry.date(),
                 price_at=pr.pubdate.iloc[0] if len(pr) else pd.NaT, exit=exit_.date() if len(pr) else None)
        last = min(today, exit_) if len(pr) else today
        days = bdays[(bdays >= entry) & (bdays <= last)]
        if r['shortable']:
            recs = [jfetch(code, str(x.date())) for x in days if x < today or x == today]
            r['flags'] = ''.join((x.get('flag') or '-') if x.get('found') else '?' for x in recs)
            fees = [x['fee_pct'] for x in recs if x.get('found') and x.get('fee_pct') is not None]
            r['fee_mean'] = float(np.mean(fees)) if fees else 0.0
        # 株価（入口の寄り・出口の引け）
        if len(pr) and exit_ <= today:
            px = c.execute(f"""SELECT Date, AdjO, AdjC, Va FROM bars WHERE Code='{code}' AND Date BETWEEN '{(entry - pd.Timedelta(days=40)).date()}' AND '{exit_.date()}' ORDER BY Date""").df()
            px['Date'] = pd.to_datetime(px.Date)
            px = px.set_index('Date')
            if entry in px.index and exit_ in px.index and entry in tp.index and exit_ in tp.index:
                ex = (px.AdjC[exit_] / px.AdjO[entry] - 1) - (tp.close[exit_] / tp.open[entry] - 1)
                va = px.Va[px.index < entry].tail(20).median()
                cost = (float(liq_cost_bp(np.nan_to_num(va))) + SLIP_BP) / 1e4
                hold = len(days)
                cal_days = (exit_ - entry).days + 1
                r['p1_ex_bp'] = ex * 1e4
                r['p1_real_bp'] = (-ex - cost - BORROW * hold / 245 - r.get('fee_mean', 0.0) / 100 * cal_days / 365) * 1e4
        rows.append(r)
    COLS = ['code', 'name', 'ann', 'shortable', 'entry', 'price_at', 'exit', 'flags', 'fee_mean', 'p1_ex_bp', 'p1_real_bp']
    new = pd.DataFrame(rows, columns=COLS) if not rows else pd.DataFrame(rows)
    for d in (old, new):
        if len(d):
            d['ann'] = pd.to_datetime(d['ann']).dt.strftime('%Y-%m-%d %H:%M:%S')
            d['code'] = d['code'].astype(str)
    st = pd.concat([old, new]).drop_duplicates(['code', 'ann'], keep='last') if len(old) else new
    st = st.reindex(columns=list(dict.fromkeys(COLS + list(st.columns))))
    st.to_csv(os.path.join(STATE, 'state.csv'), index=False)
    write_note(st, args.vault)
    print(f'前向きの事象 {len(st)} 件（今回見た {len(new)} 件）')


def write_note(st, vault):
    st = st.copy()
    st['ann'] = pd.to_datetime(st['ann'])
    st = st.sort_values('ann', ascending=False)
    L = ['---', 'type: research', 'domain: 需給', 'project: 新戦術', 'market: 日本株', 'system: wbjp', 'status: 前向き',
         f'updated: {date.today()}', 'tags:', '  - 検証', '---', '',
         '# 公募増資の P1 の前向きの記録', '',
         '[[2026-09-jp-public-offering]] の P1（発表後の寄りで売り → 条件決定の日の引けで買い戻す）を、2026-09-26 より後の発表で紙の上で追う。',
         '`test/po_forward.py` が毎営業日 19:45 に作り直す（手で書き足さない）。発注はしていない。', '',
         '- 制限: 入口から出口までの各営業日の日証金の状態（`-` なし、`注` 注意喚起、`停` 申込停止、`?` 行なし）。入口の日が `停` なら建てられない',
         '- 逆日歩: 保有日の品貸料率の年率換算の平均（%）',
         '- P1 実額: TOPIX 超過の売り − 流動性別コスト − 滑り 10 bp − 貸株料 年 3.1% − 逆日歩（bp）', '']
    L.append('| 発表 | コード | 銘柄 | 貸借 | 入口 | 条件決定 | 制限 | 逆日歩 % | P1 超過 | P1 実額 |')
    L.append('|---|---|---|---|---|---|---|--:|--:|--:|')
    for _, r in st.iterrows():
        f = lambda k, fmt='{:+.0f}': (fmt.format(r[k]) if k in r and pd.notna(r[k]) else '')  # noqa: E731
        L.append(f"| {r.ann:%Y-%m-%d %H:%M} | {r.code} | {r['name']} | {'○' if r.shortable in (True, 'True') else '×'} | {r.entry} | "
                 f"{r.exit if isinstance(r.exit, str) or pd.notna(r.exit) else '未'} | {r.get('flags', '') if pd.notna(r.get('flags', '')) else ''} | "
                 f"{f('fee_mean', '{:.2f}')} | {f('p1_ex_bp')} | {f('p1_real_bp')} |")
    sh = st[st.shortable.astype(str) == 'True']
    done = sh.dropna(subset=['p1_real_bp']) if 'p1_real_bp' in sh else sh.iloc[0:0]
    if len(done):
        ok = done[~done['flags'].astype(str).str.startswith('停')]
        msg = f'貸借で決着した {len(done)} 件のうち、入口の日に申込停止 {len(done) - len(ok)} 件。'
        if len(ok):
            msg += f'建てられた {len(ok)} 件の P1 実額の平均 {ok.p1_real_bp.mean():+.1f} bp（中央値 {ok.p1_real_bp.median():+.1f}）。'
        L += ['', msg]
    open(os.path.join(vault, NOTE), 'w').write('\n'.join(L) + '\n')


if __name__ == '__main__':
    main()
