"""JPX日経400・JPX日経中小型株指数の定期入替の発表 PDF（「…構成銘柄の定期入替について」）を読む。

  from jpx_rebalance_pdf import parse
  r = parse('data_j.pdf')   # → {'j400': {'adds': [...], 'dels': [...]}, 'jsm': {...}, 'eff': '2026-08-31', 'checked': True}

- 各指数の節の「追加銘柄一覧」「除外銘柄一覧」の表から銘柄コードを読み、件数を本文の「○銘柄を追加、○銘柄を除外」と突き合わせる
  （2017〜2026 の 9 回で一致を確かめた。2015・2016 は中小型株指数が無い書式）。
- 実施日は本文の「定期入替実施日」。
- 脚注の「4842: USEN」のような非定期の除外のコードは拾わない。
"""
import re


def _text(path):
    from pypdf import PdfReader
    return '\n'.join(p.extract_text() for p in PdfReader(path).pages).replace('　', ' ')


def _codes(x):
    x = re.sub(r'\d{3}[0-9A-Z]\s*[:：]', '', x)
    x = re.sub(r'\d{4}\s*年', '', x)
    return list(dict.fromkeys(re.findall(r'(?:^|\n|\s)(\d{3}[0-9A-Z])(?=\s+\S)', x)))


def _split(sec):
    k = re.search(r'(②|\d\.)\s*除外銘柄', sec)
    return _codes(sec[:k.start()]), _codes(sec[k.start():])


def parse(path):
    t = _text(path)
    flat = t.replace('\n', '')
    out = {}
    m4 = re.search(r'1\.\s*JPX\s*日経インデックス\s*400', t)
    ms = re.search(r'2\.\s*JPX\s*日経中小型株指数', t)
    if not (m4 and ms):
        raise ValueError('節の見出しが見つからない（書式が変わった？）')
    tail = re.search(r'\d\.\s*(定期入替実施日|実施日)|以\s*上', t[ms.end():])
    sec4 = t[m4.end():ms.start()]
    secs = t[ms.end():ms.end() + tail.start()] if tail else t[ms.end():]
    out['j400'] = dict(zip(('adds', 'dels'), _split(sec4)))
    out['jsm'] = dict(zip(('adds', 'dels'), _split(secs)))
    h4 = re.search(r'400\s*は\s*(\d+)\s*銘柄を(?:追加|新規採用|採用)し?[、，,]?\s*(\d+)\s*銘柄を除外', flat)
    hs = re.search(r'中小\s*型株指数\s*は\s*(\d+)\s*銘柄を(?:追加|新規採用|採用)し?[、，,]?\s*(\d+)\s*銘柄を除外', flat)
    eff = re.search(r'(?:定期入替実施日|実施日)[^0-9]*(20\d\d)\s*年\s*(\d+)\s*月\s*(\d+)\s*日', flat)
    out['eff'] = f'{eff.group(1)}-{int(eff.group(2)):02d}-{int(eff.group(3)):02d}' if eff else None
    out['head'] = {'j400': h4.groups() if h4 else None, 'jsm': hs.groups() if hs else None}
    ok = eff is not None
    for k, h in out['head'].items():
        ok &= h is not None and (int(h[0]), int(h[1])) == (len(out[k]['adds']), len(out[k]['dels']))
    out['checked'] = bool(ok)
    return out
