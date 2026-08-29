"""運用者への通知。cron の中で起きたことを、ログを開かなくても気づけるようにする。

送り先は環境変数 ``WBJP_ALERT_WEBHOOK_URL``（Slack / Discord の Incoming Webhook）。
未設定ならエラーログに出すだけ。通知の失敗で本処理を止めないため、
例外は握りつぶしてログに残す。
"""

from __future__ import annotations

import json
import os
import urllib.request

from wbcore.logging import get_logger

log = get_logger(__name__)

WEBHOOK_ENV = "WBJP_ALERT_WEBHOOK_URL"


def alert(title: str, body: str = "") -> bool:
    """通知を送る。送れたら True。Webhook 未設定なら False（ログには残す）。"""
    text = f"[wbjp] {title}" + (f"\n{body}" if body else "")
    url = os.environ.get(WEBHOOK_ENV)
    if not url:
        log.error("通知先が未設定のためログのみ", title=title, body=body, env=WEBHOOK_ENV)
        return False
    # Slack は text、Discord は content を読む。両方入れておけばどちらでも通る
    payload = json.dumps({"text": text, "content": text}).encode()
    request = urllib.request.Request(
        url, data=payload, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(request, timeout=10) as response:
            ok = bool(200 <= int(response.status) < 300)
    except Exception as exc:
        log.error("通知の送信に失敗", title=title, error=str(exc))
        return False
    if not ok:
        log.error("通知先がエラーを返した", title=title)
    return ok
