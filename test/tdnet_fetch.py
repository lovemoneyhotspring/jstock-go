"""TDnet の適時開示の一覧（日付・時刻・コード・表題）を、やのしんの TDnet WebAPI から日ごとに集める。

  test/.venv/bin/python test/tdnet_fetch.py 2016-08-01 2026-09-25

出力: test/out/tdnet/<YYYYMMDD>.json（取れた日は上書きしない）と test/out/tdnet.parquet（全日を束ねたもの）。
1 日 1 回の要求で、要求の間は 0.7 秒あける。公募増資・市場区分の変更などの発表日を拾う材料。
"""
import json
import os
import sys
import time
import urllib.request

import pandas as pd

OUT = os.path.join(os.path.dirname(__file__), 'out')
DIR = os.path.join(OUT, 'tdnet')


def fetch(day):
    p = os.path.join(DIR, day.strftime('%Y%m%d') + '.json')
    if os.path.exists(p):
        return False
    url = f'https://webapi.yanoshin.jp/webapi/tdnet/list/{day:%Y%m%d}.json?limit=10000'
    for i in range(4):
        try:
            with urllib.request.urlopen(url, timeout=60) as r:
                d = json.load(r)
            break
        except Exception as e:  # noqa: BLE001
            print(day.date(), 'retry', e, flush=True)
            time.sleep(2 ** (i + 1))
    else:
        return True
    items = [x.get('Tdnet', x) for x in d.get('items', [])]
    if len(items) != d.get('total_count', len(items)):
        print(day.date(), '件数の不一致', len(items), d.get('total_count'), flush=True)
    with open(p, 'w') as f:
        json.dump([{k: x.get(k) for k in ('id', 'pubdate', 'company_code', 'company_name', 'title', 'document_url')}
                   for x in items], f, ensure_ascii=False)
    return True


def main():
    os.makedirs(DIR, exist_ok=True)
    a, b = pd.Timestamp(sys.argv[1]), pd.Timestamp(sys.argv[2])
    for day in pd.date_range(a, b):
        if fetch(day):
            time.sleep(0.7)
    rows = []
    for f in sorted(os.listdir(DIR)):
        rows += json.load(open(os.path.join(DIR, f)))
    df = pd.DataFrame(rows).drop_duplicates('id')
    df['pubdate'] = pd.to_datetime(df.pubdate)
    df.to_parquet(os.path.join(OUT, 'tdnet.parquet'), index=False)
    print(len(df), df.pubdate.min(), df.pubdate.max())


if __name__ == '__main__':
    main()
