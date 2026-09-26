"""日証金（taisyaku.jp）の銘柄検索から、申込日ごとの貸借取引の状態を引く。

  from jsf_lending import fetch
  fetch('2811', '2024-06-17')  # → {'flag': '', 'loan': 48400, 'lend': 506000, 'max_fee': 6.8, 'fee': 0.10, 'fee_pct': 1.09, 'rank': 'E', 'kind': '貸借'}

- flag: 制限措置等（'停' = 申込停止、'注' = 注意喚起、'' = なし）。その申込日の時点の状態（2026-09-26 にタマホームで確認）。
- fee: 品貸料率（逆日歩、円/株/日）、fee_pct: その年率換算（%）。'-' は None。
- 履歴は 2023-04 から（それより前は空）。東証の行だけ読む。
結果は test/out/jsf/<code>_<YYYYMMDD>.json に置き、2 回目からはそれを読む。要求の間は 1 秒あける。
"""
import html
import http.cookiejar
import json
import os
import re
import time
import urllib.parse
import urllib.request

DIR = os.path.join(os.path.dirname(__file__), 'out', 'jsf')
BASE = 'https://www.taisyaku.jp/app/stock/'
UA = {'User-Agent': 'Mozilla/5.0'}


def _num(x):
    x = x.replace(',', '')
    try:
        return float(x)
    except ValueError:
        return None


def _get(code, day):
    cj = http.cookiejar.CookieJar()
    op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
    s = op.open(urllib.request.Request(BASE, headers=UA), timeout=30).read().decode('utf-8', 'ignore')
    tok = re.search(r'name="csrf_test_name" value="([^"]+)"', s).group(1)
    d = day.replace('-', '')
    body = urllib.parse.urlencode({'csrf_test_name': tok, 'mgrCd[]': code, 'mgrMei': '',
                                   'mkYmd': f'{d[:4]} / {d[4:6]} / {d[6:]}'}).encode()
    req = urllib.request.Request(BASE + 'search', data=body, headers={**UA, 'Referer': BASE})
    s = op.open(req, timeout=30).read().decode('utf-8', 'ignore')
    t = re.sub(r'<script.*?</script>', ' ', s, flags=re.S)
    t = re.sub(r'<[^>]+>', ' ', t)
    t = re.sub(r'\s+', ' ', html.unescape(t))
    # 行: コード 東証 基準日 [停|注] 融資 貸株 差引 最高料率 品貸料率 年率 ランク 銘柄名 貸借区分 最低料率
    m = re.search(rf'\b{code} 東証 (\d{{8}}) (?:(停|注) )?([\d,\-]+) ([\d,\-]+) ([\d,\-]+) ([\d.\-]+) ([\d.\-]+) ([\d.\-]+) (\S+) (\S+) (貸借融資|貸借)', t)
    if not m:
        return {'found': False}
    g = m.groups()
    return {'found': True, 'flag': g[1] or '', 'loan': _num(g[2]), 'lend': _num(g[3]), 'max_fee': _num(g[5]),
            'fee': _num(g[6]), 'fee_pct': _num(g[7]), 'rank': g[8] if g[8] != '-' else '', 'kind': g[10]}


def fetch(code, day):
    code = str(code)[:4]
    os.makedirs(DIR, exist_ok=True)
    p = os.path.join(DIR, f'{code}_{day.replace("-", "")}.json')
    if os.path.exists(p):
        return json.load(open(p))
    for i in range(4):
        try:
            r = _get(code, day)
            break
        except Exception as e:  # noqa: BLE001
            print('jsf retry', code, day, e, flush=True)
            time.sleep(2 ** (i + 1))
    else:
        return {'found': False, 'error': True}
    json.dump(r, open(p, 'w'), ensure_ascii=False)
    time.sleep(1.0)
    return r


if __name__ == '__main__':
    import sys
    print(fetch(sys.argv[1], sys.argv[2]))
