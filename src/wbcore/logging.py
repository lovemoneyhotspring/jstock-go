"""構造化ログと秘匿情報のマスク。

**なぜこれが必須か**

Webull SDK は API がエラーを返すと、リクエストヘッダを丸ごとログに出力する。
そこには ``x-app-key`` と ``x-signature`` が含まれる。何もしなければ、
API エラーが1回起きるだけでログファイルに認証情報が残る。

そのため、マスクは「オプション」ではなくログ経路の必須の一部として組む。
アプリのログだけでなく、**SDK が出す stdlib logging の出力も含めて**
最終的な文字列の段階で必ず通す。

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

#: マスク対象のキー名。SDK が出す JSON 形式とヘッダ形式の両方に効かせる。
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
        except TypeError, ValueError:
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
LOG_SCHEMA = "wbjp.log.v1"


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
                processor=structlog.processors.JSONRenderer(ensure_ascii=False, sort_keys=True),
                foreign_pre_chain=shared_processors,
            )
        )
        file_handler.addFilter(RedactingFilter())
        root.addHandler(file_handler)

    for name in quiet_loggers:
        logging.getLogger(name).setLevel(logging.WARNING)

    # SDK が自前のハンドラでマスクを迂回するのを塞ぐ。SDK はインポート時に
    # 設定するため、接続直前にも改めて呼ぶ必要がある（webull.py を参照）。
    harden_third_party_logging()


class _RedactingProcessorFormatter(structlog.stdlib.ProcessorFormatter):
    """structlog の ProcessorFormatter に最終マスクを足したもの。"""

    def format(self, record: logging.LogRecord) -> str:
        return redact(super().format(record))


def get_logger(name: str) -> structlog.stdlib.BoundLogger:
    """名前付きの構造化ロガーを返す。"""
    return structlog.stdlib.get_logger(name)


def suppress_sdk_own_logging(api_client: Any) -> None:
    """Webull SDK が独自のログ出力を仕込むのを抑止する。**``ApiClient`` を作った直後、
    ``TradeClient`` / ``DataClient`` に渡す前に必ず呼ぶ。**

    **なぜ必要か（実測で確認した挙動）**

    ``TradeClient.__init__`` と ``DataClient.__init__`` は ``_init_logger`` を呼び、
    ログが未設定だと判断すると次の2つを勝手に追加する:

        1. stdout への ``StreamHandler``
        2. **カレントディレクトリの** ``webull_trade_sdk.log`` /
           ``webull_data_sdk.log`` への ``TimedRotatingFileHandler``（INFO、72世代）

    どちらも ``propagate`` とは無関係にこちらのマスク経路を通らない。
    API がエラーを返すと SDK はリクエストヘッダ（``x-app-key`` と
    ``x-signature``）を丸ごと出力するため、**認証情報が平文でディスクに残る**。
    実際に、抑止を通さずに ``DataClient`` を作っていた経路で
    ``webull_data_sdk.log`` に ``x-app-key`` が書き出されていた。

    ``_init_logger`` は ``_stream_logger_set`` と ``_file_logger_set`` の
    いずれかが真なら何もしない。そこで構築前に立てておく。
    非公開属性だが、これが SDK 側に用意された唯一の抑止経路であり、
    代替はディスクへの認証情報の書き出しを許すことになる。
    """
    api_client._stream_logger_set = True
    api_client._file_logger_set = True


#: マスク経路を迂回されると困るサードパーティのロガー接頭辞。
_THIRD_PARTY_PREFIXES = ("webull",)


def harden_third_party_logging() -> list[str]:
    """マスクを迂回する外部ロガーを無力化する。

    **なぜ必要か**

    Webull SDK の ``webull.core.http.response`` は、インポートされた時点で
    自前の ``StreamHandler`` を DEBUG レベルで追加し、さらに
    ``propagate = False`` を設定する。この状態だと、そのモジュールのログは
    ルートロガーに一切届かず、:class:`RedactingFormatter` を通らないまま
    stderr へ直接書き出される。SDK はリクエストヘッダ（``x-app-key`` と
    ``x-signature`` を含む）を出力するので、放置すれば認証情報が漏れる。

    そこで対象ロガーのハンドラを取り除き、``propagate`` を戻して、
    こちらが用意した経路を必ず通るようにする。

    SDK はインポート時に設定するため、**SDK を読み込んだ後**にも
    呼ぶ必要がある。:meth:`configure_logging` と、実際に SDK へ接続する
    直前の両方から呼んでいる。

    Returns:
        無力化したロガー名。
    """
    hardened = []
    manager = logging.Logger.manager

    for name in list(manager.loggerDict):
        if not name.startswith(_THIRD_PARTY_PREFIXES):
            continue
        logger = logging.getLogger(name)
        if logger.handlers or not logger.propagate:
            for handler in logger.handlers[:]:
                logger.removeHandler(handler)
            logger.propagate = True
            logger.addFilter(RedactingFilter())
            hardened.append(name)

    # SDK の初期化はトークン確認の結果を INFO で出す（"_check_token_enable result
    # is False"）。運用ログには意味が無く、しかも最初の 1 行は SDK が自前の
    # ハンドラを付けた直後に出るためマスク経路を通らない。レベルで落とす。
    logging.getLogger("webull.core.http.initializer").setLevel(logging.WARNING)

    return hardened
