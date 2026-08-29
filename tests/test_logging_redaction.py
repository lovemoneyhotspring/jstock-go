"""秘匿情報マスクのテスト。

ここが破れると認証情報がログに残る。ゆるいテストでは意味がないので、
**実際に Webull SDK が出力したエラーログ**をそのまま素材に使う。
"""

from __future__ import annotations

import logging

import pytest

from wbcore.logging import (
    REDACTED,
    RedactingFormatter,
    clear_secrets,
    configure_logging,
    redact,
    redact_processor,
    register_secret,
)

# 実際に webull.core.client が認証失敗時に出力したログ（値は差し替え済み）。
# ヘッダを丸ごと吐くため、何もしなければここから app key と署名が漏れる。
REAL_SDK_ERROR_LOG = """\
ServerException occurred. Host:api.webull.co.jp SDK-Version:2.0.17 Request:{
  "_version": "v2",
  "_action_name": "/openapi/config",
  "_header": {
    "x-version": "v2",
    "x-app-key": "209fffb82d4e62b60d167b7b9c55e163",
    "x-timestamp": "2026-08-24T13:43:38Z",
    "x-signature-version": "1.0",
    "x-signature-algorithm": "HMAC-SHA256",
    "x-signature-nonce": "a496db6b-2445-5ed8-aae8-837e0160a377",
    "x-signature": "rarYtIfemMWMwFdSB10VKCHfo93LThHSMTquamw32SY=",
    "x-webull-client-source": "sdk"
  }
} Response:{'error_code': 'UNAUTHORIZED'}"""

APP_KEY = "209fffb82d4e62b60d167b7b9c55e163"
APP_SECRET = "af02275fc2e9cfccd3745c85f48b40cd"
SIGNATURE = "rarYtIfemMWMwFdSB10VKCHfo93LThHSMTquamw32SY="
ACCOUNT_ID = "1241489592734023680"


@pytest.fixture(autouse=True)
def _clean_secrets() -> None:
    clear_secrets()


def test_redacts_real_sdk_error_log() -> None:
    """SDK の実エラーログから app key と署名が消えること。"""
    result = redact(REAL_SDK_ERROR_LOG)
    assert APP_KEY not in result
    assert SIGNATURE not in result
    assert REDACTED in result


def test_redaction_keeps_diagnostic_context() -> None:
    """マスクしても、デバッグに要る情報は残す。"""
    result = redact(REAL_SDK_ERROR_LOG)
    assert "UNAUTHORIZED" in result
    assert "/openapi/config" in result
    assert "HMAC-SHA256" in result  # アルゴリズム名は秘密ではない


@pytest.mark.parametrize(
    "line",
    [
        f'"x-app-key": "{APP_KEY}"',
        f"x-app-key: {APP_KEY}",
        f"x-app-key={APP_KEY}",
        f"'app_secret': '{APP_SECRET}'",
        f'"appSecret":"{APP_SECRET}"',
        f'"account_id": "{ACCOUNT_ID}"',
        '"accountNumber": "CJP0137909"',
        f'"x-access-token": "{SIGNATURE}"',
    ],
)
def test_redacts_each_sensitive_key_form(line: str) -> None:
    """JSON・ヘッダ・クエリなど、書式が違っても捉える。"""
    result = redact(line)
    for secret in (APP_KEY, APP_SECRET, ACCOUNT_ID, SIGNATURE, "CJP0137909"):
        assert secret not in result, f"{line!r} -> {result!r}"


def test_registered_secret_is_redacted_anywhere() -> None:
    """キー名が付いていない裸の値でも、登録済みなら必ず消える。

    パターンの取りこぼしを塞ぐ最後の砦。
    """
    register_secret(APP_SECRET)
    result = redact(f"謎のエラー: {APP_SECRET} が原因かもしれません")
    assert APP_SECRET not in result
    assert REDACTED in result


def test_short_values_are_not_registered() -> None:
    """短い値を登録すると無関係な文字列まで潰れるため、無視する。"""
    register_secret("abc")
    assert redact("abc def") == "abc def"


def test_register_secret_ignores_none() -> None:
    register_secret(None)
    assert redact("そのまま") == "そのまま"


def test_redact_handles_empty() -> None:
    assert redact("") == ""


# --------------------------------------------------------------------------
# ログ経路の統合テスト
# --------------------------------------------------------------------------


def test_formatter_redacts_exception_traceback() -> None:
    """例外のトレースバック経由の漏洩も止める。

    SDK は認証情報を含むリクエスト内容を例外メッセージに載せるため、
    メッセージ本体だけを見ていては不十分。
    """
    formatter = RedactingFormatter("%(message)s")
    try:
        raise RuntimeError(REAL_SDK_ERROR_LOG)
    except RuntimeError:
        import sys

        record = logging.LogRecord(
            name="webull.core.client",
            level=logging.ERROR,
            pathname=__file__,
            lineno=1,
            msg="リクエスト失敗",
            args=(),
            exc_info=sys.exc_info(),
        )

    output = formatter.format(record)
    assert APP_KEY not in output
    assert SIGNATURE not in output


def test_sdk_logger_output_is_redacted(capsys: pytest.CaptureFixture[str]) -> None:
    """SDK のロガーが出力しても漏れないこと（経路全体の確認）。"""
    register_secret(APP_SECRET)
    configure_logging("INFO")

    # SDK と同じロガー名で、SDK と同じ形の内容を出す
    logging.getLogger("webull.core.client").error(REAL_SDK_ERROR_LOG)
    logging.getLogger("webull.core.client").error("secret=%s", APP_SECRET)

    captured = capsys.readouterr().err
    assert APP_KEY not in captured
    assert APP_SECRET not in captured
    assert SIGNATURE not in captured
    assert REDACTED in captured


def test_structlog_output_is_redacted(capsys: pytest.CaptureFixture[str]) -> None:
    """構造化ログのフィールドもマスクされること。"""
    from wbcore.logging import get_logger

    configure_logging("INFO")
    get_logger("wbjp.test").info("発注", app_key=APP_KEY, symbol="7203")

    captured = capsys.readouterr().err
    assert APP_KEY not in captured
    assert "7203" in captured  # 秘密でない情報は残る


def test_redact_processor_masks_sensitive_keys() -> None:
    event = {"event": "order", "app_key": APP_KEY, "symbol": "7203"}
    result = redact_processor(None, "info", event)
    assert result["app_key"] == REDACTED
    assert result["symbol"] == "7203"


def test_redact_processor_recurses_into_nested_structures() -> None:
    register_secret(APP_SECRET)
    event = {
        "event": "req",
        "payload": {"headers": [f"x-app-key: {APP_KEY}"], "body": APP_SECRET},
    }
    result = redact_processor(None, "info", event)
    flattened = str(result)
    assert APP_KEY not in flattened
    assert APP_SECRET not in flattened
