"""JPX日経中小型株指数・JPX日経400 の構成銘柄の表を、定期入替の発表資料の「全構成銘柄一覧」から作る。

  test/.venv/bin/python test/jsm_members.py <PDF を置いたディレクトリ>

入力: 各年の定期入替の発表ページ（j400_rebalance.py の docstring の URL）に付く全構成銘柄一覧の PDF。
  中小型: j_kousei3.pdf（2017）/ j-kousei3.pdf（2018）/ componentms_jp.pdf（2019）/ data4_j.pdf（2021〜2025）/ mei2_1_jpxsmall.pdf（2026）
  400:    J_kousei2.pdf / j-kousei2.pdf / component400_jp.pdf / data3_j.pdf / mei2_1_jpx400.pdf
  ファイル名は m_<発表日 YYYYMMDD>_<元の名前>.pdf で置く。
2020 年の資料は無いので、2020-08 〜 2021-08 の構成銘柄は「2021 年の一覧 − 2021 年の採用 + 2021 年の除外」で作り直す
（jsm_rebalance.py・j400_rebalance.py の EVENTS）。非定期の除外（上場廃止など）は反映しない。
件数が一覧の「構成銘柄数」と一致することを確かめる。

出力: test/data/jpx_index_members.csv（index, start, end, code）。start は定期入替の実施日、end は次の実施日の前日。
"""
import glob
import os
import re
import sys

import pandas as pd

sys.path.insert(0, os.path.dirname(__file__))
from jsm_rebalance import EVENTS as JSM  # noqa: E402
from j400_rebalance import EVENTS as J400  # noqa: E402

FILES = {'jsm': r'(kousei3|componentms|data4_j|mei2_1_jpxsmall)', 'j400': r'(kousei2|component400|data3_j|mei2_1_jpx400)'}
OUTP = os.path.join(os.path.dirname(__file__), 'data', 'jpx_index_members.csv')


def read_list(path):
    from pypdf import PdfReader
    t = '\n'.join(p.extract_text() for p in PdfReader(path).pages).replace('　', ' ')
    n = int(re.search(r'構成銘柄数[:：]\s*(\d+)', t).group(1))
    t = re.sub(r'\d{4}年\d+月\d+日', '', t)
    codes = re.findall(r'(?:^|\n|\s)(\d{3}[0-9A-Z])(?=\s+[12MJPSGＭＪＰＳＧ１２]\s)', t)
    return n, list(dict.fromkeys(codes))


def main():
    d = sys.argv[1]
    eff = {}   # 発表日 → 実施日
    for ev in (JSM, J400):
        for ann, e, *_ in ev:
            eff[ann.replace('-', '')] = e
    rows = []
    lists = {}
    for idx, pat in FILES.items():
        for p in sorted(glob.glob(os.path.join(d, 'm_*.pdf'))):
            if not re.search(pat, os.path.basename(p)):
                continue
            ann = os.path.basename(p)[2:10]
            if ann not in eff:
                continue
            n, codes = read_list(p)
            print(idx, ann, '構成銘柄数', n, '読めた', len(codes), 'OK' if n == len(codes) else '不一致', flush=True)
            if n != len(codes):
                raise SystemExit('件数が一致しない')
            lists[(idx, ann)] = codes
    # 2020-08 〜 2021-08 を 2021 年の一覧から作り直す
    for idx, ev in (('jsm', JSM), ('j400', J400)):
        e21 = [x for x in ev if x[0] == '2021-08-06'][0]
        post = set(lists[(idx, '20210806')])
        pre = (post - set(e21[2].split())) | set(e21[3].split())
        lists[(idx, '2020')] = sorted(pre)
        print(idx, '2020 を作り直した', len(pre))
    starts = {'2020': '2020-08-31'}
    for idx in FILES:
        keys = sorted(k for k in lists if k[0] == idx)
        st = [(starts.get(k[1]) or eff[k[1]], k) for k in keys]
        st.sort()
        for i, (s, k) in enumerate(st):
            end = (pd.Timestamp(st[i + 1][0]) - pd.Timedelta(days=1)).strftime('%Y-%m-%d') if i + 1 < len(st) else '2099-12-31'
            rows += [dict(index=idx, start=s, end=end, code=c) for c in lists[k]]
    df = pd.DataFrame(rows)
    os.makedirs(os.path.dirname(OUTP), exist_ok=True)
    df.to_csv(OUTP, index=False)
    print(df.groupby(['index', 'start']).size().to_string())


if __name__ == '__main__':
    main()
