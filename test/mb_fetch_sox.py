#!/usr/bin/env python3
"""SOX（フィラデルフィア半導体指数）の日足を取る。

stooq は JS チャレンジ、Yahoo は 429 で塞がれているので Nasdaq の API を使う。
指数 ^SOX が取れなければ ETF の SOXX で代用する（相関はほぼ 1）。
SOXX は 2024 年に株式分割しており Nasdaq の終値は調整前なので、
1 日で |35%| を超える変化は分割とみなして係数で戻す。

出力: test/out/sox.csv（date,close の昇順）
"""

import json
import os
import urllib.request

OUT = os.path.expanduser("~/jstock-go/test/out")
UA = ("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/120.0 Safari/537.36")
FROM, TO = "2016-01-01", "2026-09-16"


def fetch(symbol: str, assetclass: str):
    url = (f"https://api.nasdaq.com/api/quote/{symbol}/historical"
           f"?assetclass={assetclass}&fromdate={FROM}&todate={TO}&limit=9999")
    req = urllib.request.Request(url, headers={"User-Agent": UA, "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        body = json.load(r)
    data = body.get("data") or {}
    rows = ((data.get("tradesTable") or {}).get("rows")) or []
    out = []
    for row in rows:
        m, d, y = row["date"].split("/")
        close = row["close"].replace("$", "").replace(",", "")
        try:
            out.append((f"{y}-{m}-{d}", float(close)))
        except ValueError:
            continue
    out.sort()
    return out


series, src = [], None
for symbol, assetclass in [("SOX", "index"), ("SOXX", "etf")]:
    try:
        series = fetch(symbol, assetclass)
    except Exception as e:                      # noqa: BLE001 — 経路が塞がれたら次を試す
        print(f"{symbol}: 取得できず（{type(e).__name__}）")
        continue
    if len(series) > 1000:
        src = symbol
        break
    print(f"{symbol}: {len(series)} 行しか取れず不採用")

if not src:
    raise SystemExit("SOX 系列を取得できなかった")

# 分割の修復: 1 日で |35%| を超える変化は価格の不連続とみなす
fixed, factor = [], 1.0
for i, (d, c) in enumerate(series):
    if i > 0:
        prev_raw = series[i - 1][1]
        if prev_raw > 0:
            ratio = c / prev_raw
            if ratio < 0.65 or ratio > 1.55:
                factor *= ratio
                print(f"分割とみなして調整: {d}  比 {ratio:.4f}")
    fixed.append((d, c / factor))

path = os.path.join(OUT, "sox.csv")
with open(path, "w", encoding="utf-8") as f:
    f.write("date,close\n")
    for d, c in fixed:
        f.write(f"{d},{c:.4f}\n")
print(f"{src}: {len(fixed)} 行 {fixed[0][0]} 〜 {fixed[-1][0]} -> {path}")
