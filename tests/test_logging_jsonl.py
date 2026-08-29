"""機械が読むログ（JSON Lines）。1 行 1 レコード、固定の項目、秘密は伏せる。"""

from __future__ import annotations

import json
import logging
from pathlib import Path

import structlog

from wbcore.logging import (
    LOG_SCHEMA,
    bind_run_context,
    configure_logging,
    get_logger,
    register_secret,
)


def _reset() -> None:
    structlog.contextvars.clear_contextvars()
    root = logging.getLogger()
    for handler in root.handlers[:]:
        handler.close()
        root.removeHandler(handler)


def test_file_log_is_json_lines_with_fixed_fields(tmp_path: Path) -> None:
    log_file = tmp_path / "logs" / "accum-uat.jsonl"
    configure_logging("INFO", timezone="Asia/Tokyo", log_file=log_file)
    run_id = bind_run_context(app="accum", env="uat", command="run")
    try:
        get_logger("demo").info("積立の判断", code="accum.decision", symbol="452A", due="25000")
        get_logger("demo").warning("足が古い", code="accum.stale", symbols={"563A": "2026-08-20"})
    finally:
        _reset()

    lines = log_file.read_text(encoding="utf-8").splitlines()
    assert len(lines) == 2
    first = json.loads(lines[0])
    assert first["event"] == "積立の判断"
    assert first["code"] == "accum.decision"
    assert first["symbol"] == "452A" and first["due"] == "25000"
    assert first["schema"] == LOG_SCHEMA
    assert first["level"] == "info" and first["logger"] == "demo"
    # 実行コンテキストが全レコードに付く
    assert first["run_id"] == run_id
    assert first["app"] == "accum" and first["env"] == "uat" and first["command"] == "run"
    # ts_utc は常に UTC、timestamp は表示の時間帯
    assert first["ts_utc"].endswith("+00:00")
    assert first["timestamp"].endswith("+09:00")
    # 鍵は辞書順（差分を取りやすく）
    assert list(first) == sorted(first)
    second = json.loads(lines[1])
    assert second["level"] == "warning" and second["symbols"] == {"563A": "2026-08-20"}


def test_file_log_redacts_secrets(tmp_path: Path) -> None:
    log_file = tmp_path / "wbjp-uat.jsonl"
    configure_logging("INFO", log_file=log_file)
    register_secret("app-key-1234567890", "very-secret-value")
    try:
        get_logger("demo").info("接続", key="app-key-1234567890", secret="very-secret-value")
    finally:
        _reset()
    text = log_file.read_text(encoding="utf-8")
    assert "very-secret-value" not in text
    assert "app-key-1234567890" not in text


def test_console_output_stays_human_readable_by_default(tmp_path: Path, capsys: object) -> None:
    """端末は整形表示のまま。JSON はファイルにだけ出る。"""
    configure_logging("INFO", log_file=tmp_path / "x.jsonl")
    try:
        get_logger("demo").info("ログの確認", code="test")
    finally:
        _reset()
    line = (tmp_path / "x.jsonl").read_text(encoding="utf-8").strip()
    assert line.startswith("{") and json.loads(line)["code"] == "test"
