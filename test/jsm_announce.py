"""JPX日経中小型株指数の定期入替の発表を見つけ、採用銘柄の注文一覧を vault に書く（毎年 8 月、deploy/jsm-announce.sh から）。

根拠: vault 20-research/2026-09-jp-jsm-rebalance.md、30-projects/季節性イベント.md。
発注は人が別の証券会社で行う（立花・daytrade とは口座を分ける）。このスクリプトは発注しない。

  test/.venv/bin/python test/jsm_announce.py [--year 2027] [--url 発表ページ] [--vault ~/obsidian-vault] [--budget 500000] [--dry-run]

1. JPX日経400 のページ（www.jpx.co.jp/markets/indices/jpx-nikkei400/）のお知らせから、その年の「…定期入替について」を探す。
   無ければ何もせず終わる（終了コード 3）。その年の注文一覧をもう書いていても終わる（終了コード 0）。
2. 発表の PDF を jpx_rebalance_pdf.parse で読む。件数が本文と合わなければ止まる（終了コード 2、人が確かめる）。
3. 対象 = 中小型の採用のうち、同じ回の JPX日経400 の採用・除外と重ならない銘柄（検証と同じ）。
4. 入口 = 発表の翌営業日の寄成、出口 = 組み入れの日（実施日の前営業日）の引成。
   買うのは売買代金（20 日中央値）の大きい順に上位 --max-names（既定 20）銘柄（vault 20-research/2026-09-jp-jsm-rank.md の R1）。
   単元数 = max(1, round(予算 ÷ 1 単元の金額))。ただし金額が売買代金 20 日中央値の 1% を超えるなら 1% に収まる単元数まで減らす（最低 1）。
5. vault の 20-research/<年>-08-jsm-adds.md に注文一覧・記入欄・検証用の EVENTS の行を書く。
"""
import argparse
import json
import os
import re
import sys
import urllib.request
from datetime import date

sys.path.insert(0, os.path.dirname(__file__))
from common import *  # noqa: E402,F403
from jpx_rebalance_pdf import parse  # noqa: E402

INDEX_PAGE = 'https://www.jpx.co.jp/markets/indices/jpx-nikkei400/index.html'
UA = {'User-Agent': 'Mozilla/5.0'}
STATE = os.path.join(OUT, 'jsm_announce')


def get(url):
    return urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=60).read()


def find_announcement(year):
    s = get(INDEX_PAGE).decode('utf-8', 'ignore')
    for h, t in re.findall(r'href="([^"]+)"[^>]*>([^<]{0,120})', s):
        m = re.search(rf'/news/\d+/({year}\d{{4}})-\d+\.html', h)
        if m and '定期入替' in t and '中小型' in t:
            return ('https://www.jpx.co.jp' + h) if h.startswith('/') else h, m.group(1)
    return None, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--year', type=int, default=date.today().year)
    ap.add_argument('--url', default='')
    ap.add_argument('--vault', default=os.path.expanduser('~/obsidian-vault'))
    ap.add_argument('--budget', type=float, default=float(os.environ.get('JSM_BUDGET_PER_NAME', 500000)))
    ap.add_argument('--max-names', type=int, default=int(os.environ.get('JSM_MAX_NAMES', 20)))
    ap.add_argument('--dry-run', action='store_true', help='vault と状態に書かず、一覧を標準出力に出す')
    a = ap.parse_args()
    os.makedirs(STATE, exist_ok=True)
    note_rel = f'20-research/{a.year}-08-jsm-adds.md'
    if not a.dry_run and os.path.exists(os.path.join(a.vault, note_rel)):
        print(f'{a.year} の注文一覧はもうある: {note_rel}')
        return 0
    if a.url:
        url, ymd = a.url, re.search(r'(\d{8})-\d+\.html', a.url).group(1)
    else:
        url, ymd = find_announcement(a.year)
        if not url:
            print(f'{a.year} の定期入替の発表はまだ無い')
            return 3
    ann = pd.Timestamp(f'{ymd[:4]}-{ymd[4:6]}-{ymd[6:]}')
    page = get(url).decode('utf-8', 'ignore')
    pdfs = re.findall(r'href="([^"]+\.pdf)"', page)
    if not pdfs:
        print('発表ページに PDF が無い', url)
        return 2
    pdf_url = ('https://www.jpx.co.jp' + pdfs[0]) if pdfs[0].startswith('/') else pdfs[0]
    pdf_path = os.path.join(STATE, f'{ymd}_{os.path.basename(pdf_url)}')
    open(pdf_path, 'wb').write(get(pdf_url))
    r = parse(pdf_path)
    if not r['checked']:
        print('件数が本文と合わない（人が確かめる）:', r['head'], {k: (len(r[k]['adds']), len(r[k]['dels'])) for k in ('j400', 'jsm')})
        return 2
    ov = set(r['j400']['adds']) | set(r['j400']['dels'])
    target = [k for k in r['jsm']['adds'] if k not in ov]

    c = con()
    cal = c.execute("SELECT Date FROM cal WHERE HolDiv='1' ORDER BY Date").df()
    bdays = pd.DatetimeIndex(pd.to_datetime(cal.Date))
    eff = pd.Timestamp(r['eff'])
    entry = bdays[bdays > ann][0]
    exit_ = bdays[bdays < eff][-1]
    codes = ','.join(repr(k + '0') for k in target)
    last = c.execute(f"SELECT max(Date) FROM bars WHERE Date <= '{ann.date()}'").fetchone()[0]
    px = c.execute(f"SELECT Code, C FROM bars WHERE Date = '{last}' AND Code IN ({codes})").df().set_index('Code').C
    va = c.execute(f"""SELECT Code, median(TRY_CAST(Va AS DOUBLE)) va FROM bars WHERE Date <= '{ann.date()}'
                       AND Date > DATE '{ann.date()}' - INTERVAL 30 DAY AND Code IN ({codes}) GROUP BY Code""").df().set_index('Code').va
    nm = c.execute(f"""SELECT Code, arg_max(CoName, Date) nm, arg_max(MktNm, Date) mk FROM master
                       WHERE Code IN ({codes}) GROUP BY Code""").df().set_index('Code')
    rows = []
    for k in target:
        code = k + '0'
        p, v = px.get(code, np.nan), va.get(code, np.nan)
        unit = p * 100 if np.isfinite(p) else np.nan
        n = max(1, int(round(a.budget / unit))) if np.isfinite(unit) else 0
        capped = False
        if n and np.isfinite(v) and n * unit > 0.01 * v:
            n2 = max(1, int(0.01 * v // unit))
            capped, n = n2 < n, n2
        rows.append(dict(code=k, name=nm.nm.get(code, ''), mkt=nm.mk.get(code, ''), close=p, unit=unit, units=n,
                         amount=n * unit if n else np.nan, va_oku=v / 1e8 if np.isfinite(v) else np.nan, capped=capped))
    df = pd.DataFrame(rows).sort_values('va_oku', ascending=False, na_position='last').reset_index(drop=True)
    df['rank'] = np.arange(1, len(df) + 1)
    df['buy'] = df['rank'] <= a.max_names
    total = df[df.buy].amount.sum()
    nb = int(df.buy.sum())
    L = ['---', 'type: research', 'domain: 需給', 'project: 新戦術', 'market: 日本株', 'system: wbjp', 'status: 前向き',
         f'date: {date.today()}', f'updated: {date.today()}', 'tags:', '  - 検証', '  - 季節性', '---', '',
         f'# {a.year} 年 8 月 JPX日経中小型株指数の採用（注文一覧）', '',
         f'`test/jsm_announce.py` が {date.today()} に作った（発表 {ann.date()}、[資料]({url})）。'
         '手順と根拠は [[季節性イベント]]・[[2026-09-jp-jsm-rebalance]]。**発注は人が別の証券会社で行う。**', '',
         '| | 日 | 注文 |', '|---|---|---|',
         f'| 入口 | **{entry.date()}**（発表の翌営業日） | 寄成で買う |',
         f'| 出口 | **{exit_.date()}**（組み入れの日 = 実施日 {eff.date()} の前営業日） | 引成で売る |', '',
         f'対象 {len(df)} 銘柄（中小型の採用 {len(r["jsm"]["adds"])} のうち、JPX日経400 の入替と重なる '
         f'{len(r["jsm"]["adds"]) - len(target)} を除く）。**買うのは売買代金の大きい順に上位 {nb} 銘柄**'
         f'（[[2026-09-jp-jsm-rank]] の R1）。1 銘柄の予算 {a.budget / 1e4:.0f} 万円、'
         f'合計 **{total / 1e4:,.0f} 万円**（{last} の終値で計算。寄りの値で変わる）。',
         '単元数 = 予算 ÷ 1 単元の金額を丸めたもの（最低 1）。売買代金 20 日中央値の 1% を超える銘柄は 1% に収まるまで減らした（印 ※）。', '',
         '| 順位 | コード | 銘柄 | 市場 | 終値 | 1 単元（万円） | 単元数 | 金額（万円） | 売買代金（億円/日） | 約定（買い） | 約定（売り） |',
         '|--:|---|---|---|--:|--:|--:|--:|--:|--:|--:|']
    for _, x in df[df.buy].iterrows():
        L.append(f"| {x['rank']} | {x.code} | {x['name']} | {x.mkt} | {x.close:,.0f} | {x.unit / 1e4:.1f} | {x.units}{' ※' if x.capped else ''} | "
                 f"{x.amount / 1e4:.1f} | {x.va_oku:.2f} |  |  |")
    rest = df[~df.buy]
    if len(rest):
        L += ['', f'買わない {len(rest)} 銘柄（{a.max_names + 1} 位以下。検証では全銘柄を測る）: '
              + '、'.join(f"{x.code} {x['name']}" for _, x in rest.iterrows())]
    L += ['', '## 検証用（組み入れの後に足す）', '',
          '9 月上旬、J-Quants に出口の日の日足が入ったら、下の行を `test/jsm_rebalance.py` と `test/j400_rebalance.py` の EVENTS に足し、'
          '`test/jsm_add_control.py`・`test/jsm_add_bias.py` を回して [[2026-09-jp-jsm-rebalance]] の「前向きの記録」に書く。', '',
          '```python',
          f"# jsm_rebalance.py\n    ('{ann.date()}', '{eff.date()}', '''\n        {' '.join(sorted(r['jsm']['adds']))}\n     ''', '''\n        {' '.join(sorted(r['jsm']['dels']))}\n     '''),",
          f"# j400_rebalance.py\n    ('{ann.date()}', '{eff.date()}', '''\n        {' '.join(sorted(r['j400']['adds']))}\n     ''', '''\n        {' '.join(sorted(r['j400']['dels']))}\n     '''),",
          '```', '']
    text = '\n'.join(L)
    summary = (f'{a.year} 年の JPX日経中小型株指数の採用: 対象 {len(df)} 銘柄のうち売買代金の上位 {nb} 銘柄を買う。'
               f'合計 {total / 1e4:,.0f} 万円（1 銘柄 {a.budget / 1e4:.0f} 万円）。'
               f'{entry.date()} の寄成で買い、{exit_.date()} の引成で売る。一覧は vault {note_rel}')
    if a.dry_run:
        print(text)
        print(summary)
        return 0
    open(os.path.join(a.vault, note_rel), 'w').write(text)
    json.dump(dict(url=url, ann=str(ann.date()), eff=r['eff'], entry=str(entry.date()), exit=str(exit_.date()),
                   n=len(df), n_buy=nb, total=total, summary=summary), open(os.path.join(STATE, f'{a.year}.json'), 'w'), ensure_ascii=False)
    print(summary)
    return 0


if __name__ == '__main__':
    sys.exit(main())
