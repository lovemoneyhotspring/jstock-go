"""CLI の出力を「人が読む表」と「AI が読む JSON」に分ける。

**なぜ**

これまで CLI の出力は Rich の表だけだった。罫線・余白・色の制御文字は、人には
読みやすいが、AI に読ませると意味の無いトークンを大量に食う。表を JSON にすると
同じ内容が 3 分の 1 以下になることが多い。

各コマンドに ``--json`` を足し、``--json`` のときは**表を一切出さず**、
1 個の JSON だけを標準出力に書く。パイプでそのまま ``jq`` に渡せるようにするため、
警告や補足も混ぜない（それらは ``notes`` に入れる）。

    daytrade review --json | jq '.rows[] | select(.picked_bp < 0)'

**金額の型**

``Decimal`` は JSON に無いので文字列にする（``docs/LOGGING.md`` の規約と同じ）。
浮動小数にすると円未満の誤差が出るため、金額は必ず文字列で渡す。
"""

from __future__ import annotations

import datetime as dt
import json
import sys
from decimal import Decimal
from pathlib import Path
from typing import Any

import polars as pl


def encode(value: Any) -> Any:
    """JSON に載らない型を落とす。金額（Decimal）は文字列のまま。"""
    match value:
        case Decimal():
            return str(value)
        case dt.datetime() | dt.date():
            return value.isoformat()
        case Path():
            return str(value)
        case pl.DataFrame():
            return rows_of(value)
        case set() | frozenset():
            return sorted(str(item) for item in value)
        case _:
            return str(value)


def rows_of(frame: pl.DataFrame) -> list[dict[str, Any]]:
    """DataFrame を行の並びに。日付や Decimal はそのまま :func:`encode` に任せる。"""
    return frame.to_dicts()


def dump(payload: dict[str, Any]) -> str:
    """JSON の文字列にする。鍵は辞書順（差分を取りやすく）。"""
    return json.dumps(payload, ensure_ascii=False, sort_keys=True, default=encode)


def emit_json(payload: dict[str, Any]) -> None:
    """標準出力に JSON を 1 個だけ書く。"""
    sys.stdout.write(dump(payload) + "\n")


def emit_error(message: str, *, code: str = "error") -> None:
    """``--json`` のときの失敗。人向けの赤字の代わりに、同じ形の JSON を出す。

    ``ok`` を必ず付けるのは、呼ぶ側が成否を 1 つの鍵で判定できるようにするため。
    """
    emit_json({"ok": False, "error": code, "message": message})
