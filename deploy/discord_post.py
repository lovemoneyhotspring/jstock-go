#!/usr/bin/env python3
"""標準入力の文章を Discord の Incoming Webhook に流す（日次レポートの配達係）。

``wbcore.notify.alert`` との違いは 2 つだけ:

- ``[wbjp]`` の接頭辞を付けない（レポートは自分で見出しを持っている）
- **2000 文字で分割する**。Discord は 1 通 2000 文字までで、超えると 400 が返り
  レポートが丸ごと消える。改行の位置で切り、切れ目にページ番号を付ける

標準ライブラリだけで動く（cron から `.venv` の外でも動かせるように）。

    cat report.md | deploy/discord_post.py
    deploy/discord_post.py --title "日次レポート" < report.md

送り先は環境変数 ``WBJP_ALERT_WEBHOOK_URL``。未設定なら何もせず 2 で終了する
（レポート本体はファイルに残っているので、配達の失敗で処理は止めない）。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

WEBHOOK_ENV = "WBJP_ALERT_WEBHOOK_URL"

#: **省略できない。** Discord は Cloudflare の後ろにいて、urllib の既定
#: （``Python-urllib/3.x``）だと本文を見ずに 403 を返す（curl だと通る）。
USER_AGENT = "wbjp/1.0 (+https://github.com/lovemoneyhotspring/jstock)"

#: Discord の 1 通の上限は 2000 文字。ページ番号（" (2/3)"）とコードブロックの
#: 閉じ直しに使う余白を引いておく。
LIMIT = 1900


def chunks(text: str, limit: int = LIMIT) -> list[str]:
    """改行の位置で ``limit`` 文字以内に切る。1 行が長すぎるときだけ行の途中で切る。"""
    out: list[str] = []
    buf = ""
    for line in text.splitlines(keepends=True):
        while len(line) > limit:  # 1 行が上限を超える異常な入力
            if buf:
                out.append(buf)
                buf = ""
            out.append(line[:limit])
            line = line[limit:]
        if len(buf) + len(line) > limit:
            out.append(buf)
            buf = ""
        buf += line
    if buf.strip():
        out.append(buf)
    return out or [text]


def post(url: str, content: str, timeout: float = 10.0) -> bool:
    """1 通送る。Discord も Slack も通るように text と content の両方を入れる。"""
    payload = json.dumps({"content": content, "text": content}).encode()
    request = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json", "User-Agent": USER_AGENT},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return 200 <= int(response.status) < 300
    except urllib.error.HTTPError as exc:
        # Discord のレート制限。Retry-After（秒）を待って 1 度だけやり直す
        if exc.code == 429:
            wait = float(exc.headers.get("Retry-After") or 1.0)
            time.sleep(min(wait, 30.0))
            return post(url, content, timeout)
        print(f"Discord がエラーを返した: {exc.code}", file=sys.stderr)
        return False
    except Exception as exc:  # 配達の失敗で呼び出し元を止めない
        print(f"Discord への送信に失敗: {exc}", file=sys.stderr)
        return False


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--title", default="", help="本文の先頭に付ける見出し")
    args = parser.parse_args()

    body = sys.stdin.read().strip()
    if not body:
        print("本文が空。送らない", file=sys.stderr)
        return 1

    url = os.environ.get(WEBHOOK_ENV)
    if not url:
        print(f"{WEBHOOK_ENV} が未設定。送らない", file=sys.stderr)
        return 2

    if args.title:
        body = f"**{args.title}**\n{body}"

    pages = chunks(body)
    ok = True
    for i, page in enumerate(pages, 1):
        suffix = f"\n_({i}/{len(pages)})_" if len(pages) > 1 else ""
        if not post(url, page + suffix):
            ok = False
        if i < len(pages):
            time.sleep(0.5)  # 連投のレート制限を避ける
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
