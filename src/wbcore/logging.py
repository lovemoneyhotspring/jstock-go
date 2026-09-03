"""構造化ログと秘匿情報のマスク。

**なぜこれが必須か**

証券会社や データ提供元の SDK・クライアントは、API がエラーを返すと
リクエストヘッダを丸ごとログに出力することがある。そこには ``x-app-key`` や
``x-signature`` が含まれる。何もしなければ、API エラーが1回起きるだけで
ログファイルに認証情報が残る。

そのため、マスクは「オプション」ではなくログ経路の必須の一部として組む。
アプリのログだけでなく、**外部ライブラリが出す stdlib logging の出力も
含めて**最終的な文字列の段階で必ず通す。

対策は二重にしてある:
    1. パターンによるマスク — ``x-app-key: ...`` のような既知の形を潰す
    2. 実値によるマスク — 実際に読み込んだ秘密の文字列そのものを潰す
       （パターンが漏れても、値が分かっていれば必ず消える）
"""

from __future__ import annotations

import logging
import re
import sys
import uuid
from collections.abc import Iterable
from logging.handlers import TimedRotatingFileHandler
from pathlib import Path
from typing import Any

import structlog

REDACTED = "***REDACTED***"

#: 実行時に登録された秘密の実値。パターンの取りこぼしを塞ぐ最後の砦。
_SECRET_VALUES: set[str] = set()

#: マスク対象のキー名。外部ライブラリが出す JSON 形式とヘッダ形式の両方に効かせる。
_SENSITIVE_KEYS = (
    "x-app-key",
    "x-signature",
    "x-access-token",
    "app_key",
    "appKey",
    "app_secret",
    "appSecret",
    "account_id",
    "accountId",
    "account_number",
    "accountNumber",
    "secret",
    "password",
    "token",
)

#: ``"key": "value"`` / ``key=value`` / ``key: value`` を捉える。
_KV_PATTERN = re.compile(
    r"""(?ix)
    (["']?(?:"""
    + "|".join(re.escape(k) for k in _SENSITIVE_KEYS)
    + r""")["']?
     \s* [:=] \s* ["']?)
    ([^"'\s,}\]&]+)
    """
)


def register_secret(*values: str | None) -> None:
    """秘密の実値を登録し、以後ログに出たら必ずマスクする。

    認証情報を読み込んだ直後に呼ぶこと。短すぎる値は誤爆するので無視する。
    """
    for value in values:
        if value and len(value) >= 8:
            _SECRET_VALUES.add(value)


def clear_secrets() -> None:
    """登録済みの秘密を消す（テスト用）。"""
    _SECRET_VALUES.clear()


def redact(text: str) -> str:
    """文字列から秘匿情報を取り除く。"""
    if not text:
        return text
    result = _KV_PATTERN.sub(lambda m: f"{m.group(1)}{REDACTED}", text)
    # 実値によるマスクは長い順に適用する（部分文字列の食い合いを防ぐ）
    for secret in sorted(_SECRET_VALUES, key=len, reverse=True):
        if secret in result:
            result = result.replace(secret, REDACTED)
    return result


def _redact_value(value: Any) -> Any:
    """ログイベントの値を再帰的にマスクする。"""
    match value:
        case str():
            return redact(value)
        case dict():
            return {k: _redact_value(v) for k, v in value.items()}
        case list():
            return [_redact_value(v) for v in value]
        case tuple():
            return tuple(_redact_value(v) for v in value)
        case _:
            return value


def redact_processor(_logger: Any, _method: str, event_dict: dict[str, Any]) -> dict[str, Any]:
    """structlog 用プロセッサ。キー名が該当するものは値ごと潰す。"""
    sensitive = {k.lower() for k in _SENSITIVE_KEYS}
    return {
        key: REDACTED if key.lower() in sensitive else _redact_value(value)
        for key, value in event_dict.items()
    }


class RedactingFormatter(logging.Formatter):
    """整形後の文字列を必ずマスクする Formatter。

    メッセージ本体だけでなく、**例外のトレースバックも通る**のが要点。
    SDK は認証情報を含むリクエスト内容を例外メッセージに載せるため、
    ここで受け止めないと漏れる。
    """

    def format(self, record: logging.LogRecord) -> str:
        return redact(super().format(record))


class RedactingFilter(logging.Filter):
    """レコード段階でマスクする Filter。

    Formatter を差し替えられないハンドラ（テストの caplog など）でも
    効くように、二重の防御として入れておく。
    """

    def filter(self, record: logging.LogRecord) -> bool:
        try:
            message = record.getMessage()
        except (TypeError, ValueError):
            return True
        redacted = redact(message)
        if redacted != message:
            record.msg = redacted
            record.args = ()
        return True


def _timestamper(timezone: str) -> Any:
    """設定の時間帯で ``timestamp`` を付ける structlog プロセッサ。"""
    from wbcore.clock import stamp_iso, zone

    tz = zone(timezone)  # 未知の名前はここで早めに弾く

    def add_timestamp(_logger: Any, _name: str, event_dict: dict[str, Any]) -> dict[str, Any]:
        event_dict["timestamp"] = stamp_iso(tz)
        return event_dict

    return add_timestamp


#: 機械が読むログの形式の版。項目を変えたら上げる（docs/LOGGING.md）。
#:
#: v2 での変更（読み手を AI に絞ったための最適化）:
#:   - ``timestamp``（表示用の時間帯）をファイル出力から外した。``ts_utc`` と
#:     同じ時刻の二重持ちで、1 行あたり約 55 バイトを占めていた。端末表示には残る
#:   - ``routine`` を追加。「動いただけ」の定型行に ``true`` が付く（:data:`ROUTINE_CODES`）
LOG_SCHEMA = "wbjp.log.v2"

#: 「動いただけ」＝読み飛ばしてよい定型イベントの ``code``。
#:
#: ここに載る行は ``routine: true`` が付き、AI は既定で読み飛ばす:
#:
#: .. code-block:: console
#:
#:     jq 'select(.routine != true)' state/logs/daytrade-prod.jsonl
#:
#: 判断・発注・異常は**絶対にここに入れない**。「いつも通り動いた」ことだけを
#: 示す行——同期で変化が無かった、時間帯の外で何もしなかった、等——に限る。
ROUTINE_CODES = frozenset(
    {
        "accum.skip",  # 発注時間帯の外
        "jquants.no_calendar",  # カレンダーが無く平日で代用
        "settings.state_migrated",  # 置き場の移動（1 回きり）
    }
)


def _mark_routine(_logger: Any, _name: str, event_dict: dict[str, Any]) -> dict[str, Any]:
    """定型行に ``routine: true`` を付ける。

    決め方は 2 通りで、呼び出し側の明示が優先する:
        1. ``log.info(..., routine=True)`` — ``code`` の無い行はこちら
        2. ``code`` が :data:`ROUTINE_CODES` にある

    ``false`` は書かない（無い＝定型ではない）。AI が読む行数を減らすのが目的なので、
    意味の無い項目を全行に足しては本末転倒になる。
    """
    explicit = event_dict.pop("routine", None)
    if explicit is True or (explicit is None and event_dict.get("code") in ROUTINE_CODES):
        event_dict["routine"] = True
    return event_dict


def _machine_fields(_logger: Any, _name: str, event_dict: dict[str, Any]) -> dict[str, Any]:
    """後から AI や集計に読ませるための固定項目。

    - ``ts_utc``: 表示の時間帯に関係なく UTC。並べ替えと突き合わせの鍵
    - ``schema``: 形式の版
    - ``code``: 出来事の安定した識別子（``accum.decision`` 等）。付いていない
      ログは ``event``（日本語の説明文）だけで分類する
    """
    from wbcore.clock import stamp_iso

    event_dict.setdefault("ts_utc", stamp_iso("UTC"))
    event_dict.setdefault("schema", LOG_SCHEMA)
    return event_dict


def bind_run_context(**fields: Any) -> str:
    """この実行のあいだ全ログに付く項目（app / env / command / config_dir …）。

    ``run_id`` を発行して返す。1回の CLI 実行のログを後から 1 本の線として
    読めるようにするためのもの。
    """
    run_id = uuid.uuid4().hex[:12]
    structlog.contextvars.bind_contextvars(run_id=run_id, **fields)
    return run_id


def current_run_id() -> str:
    """いま束ねている実行の ``run_id``。:func:`bind_run_context` の前なら空文字。

    ログ以外の記録（選定の履歴など）に同じ ID を付け、ログと突き合わせられるようにする。
    """
    value = structlog.contextvars.get_contextvars().get("run_id", "")
    return str(value)


def configure_logging(
    level: str = "INFO",
    *,
    json_output: bool = False,
    timezone: str = "UTC",
    log_file: Path | None = None,
    quiet_loggers: Iterable[str] = ("urllib3", "asyncio", "botocore"),
) -> None:
    """ログ経路を構築する。

    structlog と stdlib logging の出力を集約し、出口に :class:`RedactingFormatter`
    を必ず置く。SDK のログもここを通るため、経路の取りこぼしが起きない。

    出口は 2 つ:
        端末（stderr）
            人が読む。既定は色付きの整形、``json_output`` で JSON。
        ファイル（``log_file``）
            **機械が読む**。常に JSON Lines（1 行 1 レコード、UTF-8）で、
            日次でローテーションする。後から AI に読ませて改善に使う前提なので、
            項目名は docs/LOGGING.md に固定してある。
    """
    shared_processors: list[Any] = [
        structlog.contextvars.merge_contextvars,
        structlog.stdlib.add_log_level,
        structlog.stdlib.add_logger_name,
        # ログの時刻は設定の時間帯（既定 UTC）。オフセット付き ISO なので、
        # どの時間帯で書かれたかがログ自身に残る
        _timestamper(timezone),
        _machine_fields,
        structlog.processors.StackInfoRenderer(),
        # 例外はここで文字列（``exception`` 項目）にする。これが無いと JSONL には
        # ``"exc_info": true`` しか残らず、後から原因を追えない。マスクの前に置き、
        # トレースバック本文（SDK がリクエストヘッダを載せる）も必ずマスクに通す
        structlog.processors.format_exc_info,
        _mark_routine,
        redact_processor,
    ]

    structlog.configure(
        processors=[
            *shared_processors,
            structlog.stdlib.ProcessorFormatter.wrap_for_formatter,
        ],
        logger_factory=structlog.stdlib.LoggerFactory(),
        wrapper_class=structlog.stdlib.BoundLogger,
        cache_logger_on_first_use=True,
    )

    renderer: Any = (
        structlog.processors.JSONRenderer(ensure_ascii=False)
        if json_output
        else structlog.dev.ConsoleRenderer(colors=sys.stderr.isatty())
    )

    formatter = _RedactingProcessorFormatter(
        processor=renderer,
        foreign_pre_chain=shared_processors,
    )

    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(formatter)
    handler.addFilter(RedactingFilter())

    root = logging.getLogger()
    for existing in root.handlers[:]:
        root.removeHandler(existing)
    root.addHandler(handler)
    root.setLevel(level.upper())

    if log_file is not None:
        log_file.parent.mkdir(parents=True, exist_ok=True)
        file_handler = TimedRotatingFileHandler(
            log_file, when="midnight", utc=True, backupCount=90, encoding="utf-8"
        )
        file_handler.setFormatter(
            _RedactingProcessorFormatter(
                processor=_machine_renderer(),
                foreign_pre_chain=shared_processors,
            )
        )
        file_handler.addFilter(RedactingFilter())
        root.addHandler(file_handler)

    for name in quiet_loggers:
        logging.getLogger(name).setLevel(logging.WARNING)


def _machine_renderer() -> Any:
    """ファイル（機械が読む側）用のレンダラ。

    端末に出す ``timestamp``（表示の時間帯）を落としてから JSON にする。
    同じ時刻は ``ts_utc`` にあり、読み手は AI なので時間帯の変換は難しくない。
    1 行あたり約 55 バイト——全体の 15% ほど——がこれだけで減る。
    """
    render = structlog.processors.JSONRenderer(ensure_ascii=False, sort_keys=True)

    def renderer(logger: Any, name: str, event_dict: dict[str, Any]) -> str:
        event_dict.pop("timestamp", None)
        rendered = render(logger, name, event_dict)
        return rendered if isinstance(rendered, str) else rendered.decode("utf-8")

    return renderer


class _RedactingProcessorFormatter(structlog.stdlib.ProcessorFormatter):
    """structlog の ProcessorFormatter に最終マスクを足したもの。"""

    def format(self, record: logging.LogRecord) -> str:
        return redact(super().format(record))


def get_logger(name: str) -> structlog.stdlib.BoundLogger:
    """名前付きの構造化ロガーを返す。"""
    return structlog.stdlib.get_logger(name)
